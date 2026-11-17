package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	snapshottypes "github.com/sei-protocol/sei-chain/sei-cosmos/snapshots/types"
	"github.com/sei-protocol/seilog"

	"github.com/sei-protocol/sei-k8s-controller/sidecar/engine"
	"github.com/sei-protocol/sei-k8s-controller/sidecar/rpc"
	seis3 "github.com/sei-protocol/sei-k8s-controller/sidecar/s3"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire"
)

var uploadLog = seilog.NewLogger("seictl", "task", "snapshot-upload")

const (
	uploadStateFile        = ".sei-sidecar-last-upload.json"
	defaultUploadInterval  = 7 * 24 * time.Hour // weekly
	verifiedArchiveVersion = 1

	// defaultUploadTimeout bounds a single one-shot upload. Uploads on large
	// chains legitimately run 60-90 min, so the default is generous; a wedged
	// S3 stream fails at the deadline rather than sitting 'running' forever.
	defaultUploadTimeout = 2 * time.Hour
)

// SnapshotUploadRequest holds the parameters for the snapshot upload task.
// S3 bucket, region, and prefix are derived from the sidecar's environment.
type SnapshotUploadRequest struct{}

// UploadOutcome and NoopReason are the snapshot-upload result-wire contract.
// They live in sidecar/wire (the dependency-free contract home) and are aliased
// here so handler call sites and the CLI poller reference one definition.
type (
	UploadOutcome = wire.UploadOutcome
	NoopReason    = wire.NoopReason
)

const (
	OutcomeUploaded = wire.OutcomeUploaded
	OutcomeNoop     = wire.OutcomeNoop
	OutcomeError    = wire.OutcomeError

	NoopNoCompletedSnapshots = wire.NoopNoCompletedSnapshots
	NoopAlreadyUploaded      = wire.NoopAlreadyUploaded
)

// SnapshotUploadResult reports local archive publication and the shared marker
// outcome. An uploaded archive may leave an equal or newer marker unchanged.
type SnapshotUploadResult struct {
	Outcome       UploadOutcome       `json:"outcome"`
	NoopReason    NoopReason          `json:"noopReason,omitempty"`
	Height        int64               `json:"height,omitempty"`
	Key           string              `json:"key,omitempty"`
	MarkerOutcome seis3.MarkerOutcome `json:"markerOutcome,omitempty"`
}

// uploadState tracks the last successfully uploaded snapshot height and when it
// was uploaded. LastUploadedAt (unix seconds) is persisted so the uploaded
// gauges can be re-emitted on sidecar startup, avoiding a false-stale reading
// after the uploading pod restarts.
type uploadState struct {
	LastUploadedHeight         int64  `json:"lastUploadedHeight"`
	LastUploadedAt             int64  `json:"lastUploadedAt,omitempty"`
	ArchiveVerificationVersion uint32 `json:"archiveVerificationVersion,omitempty"`
	Bucket                     string `json:"bucket,omitempty"`
	ChainID                    string `json:"chainId,omitempty"`
}

// verifiesArchive reports whether this cache records a verified archive for the destination.
// Older checkpoints cannot establish the archive completion or hash contract.
func (s uploadState) verifiesArchive(bucket, chainID string) bool {
	return s.ArchiveVerificationVersion == verifiedArchiveVersion && s.Bucket == bucket && s.ChainID == chainID
}

type snapshotLister interface {
	ListSnapshots(context.Context) ([]snapshottypes.Snapshot, error)
}

// SnapshotUploader publishes snapshots completed by the local producer to S3.
type SnapshotUploader struct {
	homeDir             string
	bucket              string
	region              string
	chainID             string
	uploadInterval      time.Duration
	s3UploaderFactory   seis3.UploaderFactory
	latestMarkerFactory seis3.LatestMarkerFactory
	snapshotClient      snapshotLister
}

// NewSnapshotUploader creates an uploader for a home and a nonempty S3 destination.
// Nil factories use the default AWS configuration for archive and marker clients.
func NewSnapshotUploader(homeDir, bucket, region, chainID string, uploadInterval time.Duration, factory seis3.UploaderFactory, markerFactory seis3.LatestMarkerFactory) (*SnapshotUploader, error) {
	if bucket == "" || region == "" || chainID == "" {
		return nil, fmt.Errorf("snapshot-upload: bucket, region, and chainID are required")
	}
	if factory == nil {
		factory = seis3.DefaultUploaderFactory
	}
	if markerFactory == nil {
		markerFactory = seis3.DefaultLatestMarkerFactory
	}
	if uploadInterval <= 0 {
		uploadInterval = defaultUploadInterval
	}
	return &SnapshotUploader{
		homeDir:             homeDir,
		bucket:              bucket,
		region:              region,
		chainID:             chainID,
		uploadInterval:      uploadInterval,
		s3UploaderFactory:   factory,
		latestMarkerFactory: markerFactory,
		snapshotClient:      rpc.NewSnapshotClient(nil),
	}, nil
}

// Handler returns an engine.TaskHandler for the snapshot-upload task.
// The handler runs in a loop, attempting an upload on each tick and
// sleeping for the configured interval between attempts. It stays
// running until the context is cancelled.
func (u *SnapshotUploader) Handler() engine.TaskHandler {
	return engine.TypedHandler(func(ctx context.Context, _ SnapshotUploadRequest) error {
		return u.runLoop(ctx)
	})
}

// OnceHandler returns an engine.TaskHandler for the one-shot snapshot-upload
// task. It runs Upload exactly once and returns the structured result so the
// task reaches a real terminal (completed with an outcome, or failed with the
// error). The execution is bounded by a handler-internal deadline so a wedged
// S3 stream fails cleanly rather than stranding the task in 'running'. The
// deadline lives on a child context: it surfaces as context.DeadlineExceeded,
// which the engine persists as Failed (its cancellation-suppression guard keys
// only on context.Canceled).
func (u *SnapshotUploader) OnceHandler(timeout time.Duration) engine.TaskHandler {
	if timeout <= 0 {
		timeout = defaultUploadTimeout
	}
	return engine.TypedHandlerWithResult(func(ctx context.Context, _ SnapshotUploadRequest) (SnapshotUploadResult, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return u.Upload(ctx)
	})
}

func (u *SnapshotUploader) runLoop(ctx context.Context) error {
	uploadLog.Info("starting snapshot upload loop", "interval", u.uploadInterval, "bucket", u.bucket)
	for {
		if _, err := u.Upload(ctx); err != nil {
			uploadLog.Warn("upload attempt failed, will retry next interval", "error", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(u.uploadInterval):
		}
	}
}

// Upload verifies and publishes the newest snapshot completed by the local node.
// The shared marker preserves any newer publication; local state caches success.
func (u *SnapshotUploader) Upload(ctx context.Context) (SnapshotUploadResult, error) {
	last := u.readUploadState()
	verifiedLast := last.verifiesArchive(u.bucket, u.chainID)
	completed, err := u.snapshotClient.ListSnapshots(ctx)
	if err != nil {
		return u.recordError(ctx), fmt.Errorf("listing completed snapshots: %w", err)
	}
	snapshot, err := pickUploadCandidate(completed)
	if err != nil {
		return u.recordError(ctx), err
	}
	if snapshot == nil {
		if last.LastUploadedHeight > 0 && !verifiedLast {
			uploadLog.Warn("historical snapshot publication remains unverified; no completed replacement is available",
				"legacy-height", last.LastUploadedHeight, "recovery-guide", "docs/operations/snapshot-publication-recovery.md")
		}
		uploadLog.Info("no completed snapshots reported by the local node, nothing to upload")
		return u.recordTerminal(SnapshotUploadResult{Outcome: OutcomeNoop, NoopReason: NoopNoCompletedSnapshots}), nil
	}
	height := int64(snapshot.Height)

	if verifiedLast && last.LastUploadedHeight >= height {
		uploadLog.Debug("height already uploaded", "height", height, "last-uploaded", last.LastUploadedHeight)
		return u.recordTerminal(SnapshotUploadResult{Outcome: OutcomeNoop, NoopReason: NoopAlreadyUploaded, Height: height}), nil
	}

	uploadLog.Info("uploading snapshot", "height", height, "bucket", u.bucket, "region", u.region)

	uploader, err := u.s3UploaderFactory(ctx, u.region)
	if err != nil {
		return u.recordError(ctx), fmt.Errorf("building S3 uploader: %w", err)
	}

	prefix := u.chainID + "/state-sync/"

	archiveKey := fmt.Sprintf("%s%d.tar.gz", prefix, height)
	uploadLog.Info("streaming archive to S3", "key", archiveKey)
	if err := u.streamUpload(ctx, uploader, archiveKey, *snapshot); err != nil {
		return u.recordError(ctx), fmt.Errorf("uploading %s: %w", archiveKey, err)
	}

	latestKey := prefix + "latest.txt"
	marker, err := u.latestMarkerFactory(ctx, u.region)
	if err != nil {
		return u.recordError(ctx), fmt.Errorf("building snapshot marker publisher: %w", err)
	}
	markerOutcome, err := marker.Advance(ctx, u.bucket, latestKey, height)
	if err != nil {
		return u.recordError(ctx), fmt.Errorf("publishing %s: %w", latestKey, err)
	}
	uploadLog.Info("resolved latest.txt publication", "key", latestKey, "height", height, "marker-outcome", markerOutcome)

	if err := u.writeUploadState(uploadState{
		LastUploadedHeight: height, LastUploadedAt: time.Now().Unix(),
		ArchiveVerificationVersion: verifiedArchiveVersion, Bucket: u.bucket, ChainID: u.chainID,
	}); err != nil {
		return u.recordError(ctx), err
	}
	if last.LastUploadedHeight > 0 && !verifiedLast {
		uploadLog.Warn("replaced unverified local upload history; historical remote archives still need inspection",
			"legacy-height", last.LastUploadedHeight, "verified-height", height, "marker-outcome", markerOutcome,
			"recovery-guide", "docs/operations/snapshot-publication-recovery.md")
	}

	return u.recordTerminal(SnapshotUploadResult{Outcome: OutcomeUploaded, Height: height, Key: archiveKey, MarkerOutcome: markerOutcome}), nil
}

// recordTerminal records a clean outcome and local archive finalization metrics.
// The uploaded gauges describe the local archive, not the shared marker.
func (u *SnapshotUploader) recordTerminal(result SnapshotUploadResult) SnapshotUploadResult {
	now := time.Now()
	snapshotUploadLastRunSuccess.WithLabelValues(u.chainID).Set(float64(now.Unix()))
	snapshotUploadOutcomes.WithLabelValues(u.chainID, string(result.Outcome)).Inc()
	if result.Outcome == OutcomeUploaded {
		snapshotUploadLastUploaded.WithLabelValues(u.chainID).Set(float64(now.Unix()))
		snapshotUploadLastUploadedHeight.WithLabelValues(u.chainID).Set(float64(result.Height))
	}
	return result
}

// recordError increments the chain-labeled outcome counter for a failed Upload
// and returns an error-tagged result so callers can `return u.recordError(ctx),
// err` in one line. It deliberately leaves last-run-success and the uploaded
// gauges untouched — those are clean-terminal-only signals — so a failing
// uploader is visible on the outcome counter without falsely refreshing health.
//
// A context.Canceled error does not tick the counter: it mirrors the engine's
// cancellation-suppression contract (a canceled task leaves no terminal record),
// so a deploy draining a node or an operator DELETE mid-upload is not counted as
// an upload failure. The predicate matches the engine's exactly — only
// context.Canceled is suppressed; a context.DeadlineExceeded (per-task timeout)
// or a genuine S3 failure still ticks.
func (u *SnapshotUploader) recordError(ctx context.Context) SnapshotUploadResult {
	if !errors.Is(ctx.Err(), context.Canceled) {
		snapshotUploadOutcomes.WithLabelValues(u.chainID, string(OutcomeError)).Inc()
	}
	return SnapshotUploadResult{Outcome: OutcomeError}
}

// EmitStartupMetrics re-emits the last-uploaded gauges from persisted state so a
// restarted sidecar does not report a false-stale reading before its first run.
// The last-run-success gauge is deliberately left unset: it is the "no clean run
// in N hours" alert signal, and re-emitting a persisted timestamp there would
// mask a genuinely stalled uploader after a restart.
func (u *SnapshotUploader) EmitStartupMetrics() {
	st := u.readUploadState()
	if st.LastUploadedHeight <= 0 || !st.verifiesArchive(u.bucket, u.chainID) {
		return
	}
	snapshotUploadLastUploadedHeight.WithLabelValues(u.chainID).Set(float64(st.LastUploadedHeight))
	if st.LastUploadedAt > 0 {
		snapshotUploadLastUploaded.WithLabelValues(u.chainID).Set(float64(st.LastUploadedAt))
	}
}

// streamUpload sends chunks and isolated metadata to S3, aborting the object
// if the archive fails validation.
func (u *SnapshotUploader) streamUpload(ctx context.Context, uploader seis3.Uploader, key string, snapshot snapshottypes.Snapshot) error {
	metadataDir, err := os.MkdirTemp("", "sei-snapshot-metadata-*")
	if err != nil {
		return fmt.Errorf("creating archive metadata directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(metadataDir) }()
	if err := writeSnapshotMetadata(metadataDir, snapshot); err != nil {
		return err
	}

	pr, pw := io.Pipe()
	defer func() { _ = pr.Close() }()
	archiveErr := make(chan error, 1)
	go func() {
		archiveErr <- writeArchive(ctx, pw, filepath.Join(u.homeDir, "data", "snapshots"), metadataDir, snapshot)
	}()

	_, uploadErr := uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket:      aws.String(u.bucket),
		Key:         aws.String(key),
		Body:        pr,
		ContentType: aws.String("application/gzip"),
	})
	// Also close after success: an uploader that returns without consuming the
	// body must not leave the archiver blocked on its next write.
	_ = pr.CloseWithError(io.ErrClosedPipe)
	archiveError := <-archiveErr
	if uploadErr != nil {
		return uploadErr
	}
	return archiveError
}

// pickUploadCandidate returns the newest completed snapshot, if any.
func pickUploadCandidate(completed []snapshottypes.Snapshot) (*snapshottypes.Snapshot, error) {
	var newest *snapshottypes.Snapshot
	for _, snapshot := range completed {
		if newest == nil || snapshot.Height > newest.Height || (snapshot.Height == newest.Height && snapshot.Format > newest.Format) {
			newest = &snapshot
		}
	}
	if newest == nil {
		return nil, nil
	}
	if err := validateSnapshotMetadata(*newest); err != nil {
		return nil, err
	}
	return newest, nil
}

// validateSnapshotMetadata checks the height, format and expected SHA256 hashes
// required to verify an archive before publication.
func validateSnapshotMetadata(snapshot snapshottypes.Snapshot) error {
	if snapshot.Height == 0 || snapshot.Height > math.MaxInt64 {
		return fmt.Errorf("invalid completed snapshot height %d", snapshot.Height)
	}
	if snapshot.Format == 0 || snapshot.Chunks == 0 {
		return fmt.Errorf("snapshot %d has no format or chunks", snapshot.Height)
	}
	if len(snapshot.Hash) != sha256.Size {
		return fmt.Errorf("snapshot %d has an invalid SHA256 hash", snapshot.Height)
	}
	if uint64(len(snapshot.Metadata.ChunkHashes)) != uint64(snapshot.Chunks) {
		return fmt.Errorf("snapshot %d chunk count does not match its metadata", snapshot.Height)
	}
	for index, hash := range snapshot.Metadata.ChunkHashes {
		if len(hash) != sha256.Size {
			return fmt.Errorf("invalid chunk hash at height %d index %d", snapshot.Height, index)
		}
	}
	return nil
}

func (u *SnapshotUploader) readUploadState() uploadState {
	data, err := os.ReadFile(filepath.Join(u.homeDir, uploadStateFile))
	if err != nil {
		return uploadState{}
	}
	var state uploadState
	if err := json.Unmarshal(data, &state); err != nil {
		return uploadState{}
	}
	return state
}

// writeUploadState persists state atomically: a temp file in the same directory
// is written, synced, and renamed over the target. A crash mid-write leaves the
// previous complete state intact rather than a torn file that readUploadState
// would parse as zero, silently re-uploading everything from height 0.
func (u *SnapshotUploader) writeUploadState(state uploadState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshaling upload state: %w", err)
	}

	tmp, err := os.CreateTemp(u.homeDir, uploadStateFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp upload state: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp upload state: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp upload state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing temp upload state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp upload state: %w", err)
	}

	if err := os.Rename(tmpName, filepath.Join(u.homeDir, uploadStateFile)); err != nil {
		return fmt.Errorf("renaming upload state: %w", err)
	}
	return nil
}
