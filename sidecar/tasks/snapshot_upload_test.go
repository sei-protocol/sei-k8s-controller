package tasks

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/prometheus/client_golang/prometheus/testutil"

	snapshottypes "github.com/sei-protocol/sei-chain/sei-cosmos/snapshots/types"

	seis3 "github.com/sei-protocol/sei-k8s-controller/sidecar/s3"
)

type mockS3Uploader struct {
	uploads map[string][]byte
}

func newMockS3Uploader() *mockS3Uploader {
	return &mockS3Uploader{uploads: make(map[string][]byte)}
}

func (m *mockS3Uploader) UploadObject(_ context.Context, input *transfermanager.UploadObjectInput, _ ...func(*transfermanager.Options)) (*transfermanager.UploadObjectOutput, error) {
	var buf bytes.Buffer
	if input.Body != nil {
		if _, err := io.Copy(&buf, input.Body); err != nil {
			return nil, err
		}
	}
	key := *input.Bucket + "/" + *input.Key
	m.uploads[key] = buf.Bytes()
	return &transfermanager.UploadObjectOutput{}, nil
}

func mockUploaderFactory(client *mockS3Uploader) seis3.UploaderFactory {
	return func(_ context.Context, _ string) (seis3.Uploader, error) {
		return client, nil
	}
}

func newFixtureSnapshotUploader(t *testing.T, home, bucket, region, chain string, interval time.Duration, factory seis3.UploaderFactory) (*SnapshotUploader, error) {
	t.Helper()
	store, metadata := openPublicationStore(t, home)
	completed, err := store.List()
	_ = metadata.Close()
	if err != nil {
		t.Fatal(err)
	}
	return newPublicationUploader(t, completed, home, bucket, region, chain, interval, factory)
}

func candidateSnapshotWithFormat(height uint64, format uint32) snapshottypes.Snapshot {
	snapshot := candidateSnapshot(height)
	snapshot.Format = format
	return snapshot
}

func candidateSnapshot(height uint64) snapshottypes.Snapshot {
	hash := sha256.Sum256([]byte("complete chunk"))
	return snapshottypes.Snapshot{Height: height, Format: 1, Chunks: 1, Hash: hash[:], Metadata: snapshottypes.Metadata{ChunkHashes: [][]byte{hash[:]}}}
}

func TestPickUploadCandidate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		completed []snapshottypes.Snapshot
		want      uint64
	}{
		{"newest completed snapshot", []snapshottypes.Snapshot{candidateSnapshot(1000), candidateSnapshot(3000), candidateSnapshot(2000)}, 3000},
		{"one completed snapshot is sufficient", []snapshottypes.Snapshot{candidateSnapshot(1000)}, 1000},
		{"no completed snapshots", nil, 0},
		{"format preserved without interpreting chunk payload", []snapshottypes.Snapshot{candidateSnapshot(1000), candidateSnapshotWithFormat(9000, 2)}, 9000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, err := pickUploadCandidate(tc.completed)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == 0 {
				if candidate != nil {
					t.Fatalf("candidate=%+v, want none", candidate)
				}
			} else if candidate == nil || candidate.Height != tc.want {
				t.Fatalf("candidate=%+v, want height %d", candidate, tc.want)
			}
		})
	}
}

func TestPickUploadCandidate_RejectsInvalidCompletionMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(*snapshottypes.Snapshot)
	}{
		{"zero height", func(s *snapshottypes.Snapshot) { s.Height = 0 }},
		{"height exceeds result range", func(s *snapshottypes.Snapshot) { s.Height = math.MaxUint64 }},
		{"zero format", func(s *snapshottypes.Snapshot) { s.Format = 0 }},
		{"zero chunks", func(s *snapshottypes.Snapshot) { s.Chunks = 0 }},
		{"invalid aggregate hash", func(s *snapshottypes.Snapshot) { s.Hash = []byte{1} }},
		{"missing chunk hash", func(s *snapshottypes.Snapshot) { s.Metadata.ChunkHashes = nil }},
		{"extra chunk hash", func(s *snapshottypes.Snapshot) {
			s.Metadata.ChunkHashes = append(s.Metadata.ChunkHashes, make([]byte, sha256.Size))
		}},
		{"invalid chunk hash", func(s *snapshottypes.Snapshot) { s.Metadata.ChunkHashes[0] = []byte{1} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := candidateSnapshot(1000)
			tc.damage(&snapshot)
			candidate, err := pickUploadCandidate([]snapshottypes.Snapshot{snapshot})
			if err == nil || candidate != nil {
				t.Fatalf("candidate=%+v error=%v, want invalid metadata rejection", candidate, err)
			}
		})
	}
}

func TestUpload_UploadsArchiveAndLatestTxt(t *testing.T) {
	homeDir := t.TempDir()
	createPublicationSnapshots(t, homeDir, 1000)

	mock := newMockS3Uploader()
	uploader, err := newFixtureSnapshotUploader(t, homeDir, "my-bucket", "eu-central-1", "testchain", 0, mockUploaderFactory(mock))
	if err != nil {
		t.Fatalf("NewSnapshotUploader: %v", err)
	}

	result, err := uploader.Upload(context.Background())
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if result.Outcome != OutcomeUploaded {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeUploaded)
	}
	if result.NoopReason != "" {
		t.Errorf("NoopReason = %q, want empty on OutcomeUploaded", result.NoopReason)
	}
	if result.Height != 1000 {
		t.Errorf("result height = %d, want 1000", result.Height)
	}
	if result.Key != "testchain/state-sync/1000.tar.gz" {
		t.Errorf("result key = %q, want testchain/state-sync/1000.tar.gz", result.Key)
	}

	if _, ok := mock.uploads["my-bucket/testchain/state-sync/1000.tar.gz"]; !ok {
		t.Error("expected archive upload at testchain/state-sync/1000.tar.gz")
	}

	latest, ok := mock.uploads["my-bucket/testchain/state-sync/latest.txt"]
	if !ok {
		t.Fatal("expected latest.txt upload")
	}
	if string(latest) != "1000" {
		t.Errorf("latest.txt = %q, want %q", string(latest), "1000")
	}
}

func TestUpload_SkipsWhenAlreadyUploaded(t *testing.T) {
	homeDir := t.TempDir()
	createPublicationSnapshots(t, homeDir, 1000)

	state := uploadState{LastUploadedHeight: 1000, ArchiveVerificationVersion: verifiedArchiveVersion, Bucket: "my-bucket", ChainID: "testchain"}
	data, _ := json.Marshal(state)
	_ = os.WriteFile(filepath.Join(homeDir, uploadStateFile), data, 0o644)

	mock := newMockS3Uploader()
	uploader, err := newFixtureSnapshotUploader(t, homeDir, "my-bucket", "eu-central-1", "testchain", 0, mockUploaderFactory(mock))
	if err != nil {
		t.Fatalf("NewSnapshotUploader: %v", err)
	}

	result, err := uploader.Upload(context.Background())
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if result.Outcome != OutcomeNoop || result.NoopReason != NoopAlreadyUploaded {
		t.Errorf("result = %+v, want noop/already-uploaded", result)
	}

	if len(mock.uploads) != 0 {
		t.Errorf("expected no uploads (already uploaded), got %d", len(mock.uploads))
	}
}

func TestUpload_UploadsNewerSnapshot(t *testing.T) {
	homeDir := t.TempDir()
	createPublicationSnapshots(t, homeDir, 1000, 2000)

	state := uploadState{LastUploadedHeight: 1000, ArchiveVerificationVersion: verifiedArchiveVersion, Bucket: "my-bucket", ChainID: "testchain"}
	data, _ := json.Marshal(state)
	_ = os.WriteFile(filepath.Join(homeDir, uploadStateFile), data, 0o644)

	mock := newMockS3Uploader()
	uploader, err := newFixtureSnapshotUploader(t, homeDir, "my-bucket", "eu-central-1", "testchain", 0, mockUploaderFactory(mock))
	if err != nil {
		t.Fatalf("NewSnapshotUploader: %v", err)
	}

	_, err = uploader.Upload(context.Background())
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	if _, ok := mock.uploads["my-bucket/testchain/state-sync/2000.tar.gz"]; !ok {
		t.Error("expected archive upload at testchain/state-sync/2000.tar.gz")
	}
}

func TestUpload_NoOpsWhenNoCompletedSnapshots(t *testing.T) {
	homeDir := t.TempDir()
	createPublicationSnapshots(t, homeDir)

	mock := newMockS3Uploader()
	uploader, err := newFixtureSnapshotUploader(t, homeDir, "my-bucket", "eu-central-1", "testchain", 0, mockUploaderFactory(mock))
	if err != nil {
		t.Fatalf("NewSnapshotUploader: %v", err)
	}

	result, err := uploader.Upload(context.Background())
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if result.Outcome != OutcomeNoop || result.NoopReason != NoopNoCompletedSnapshots {
		t.Errorf("result = %+v, want noop/no-completed-snapshots", result)
	}

	if len(mock.uploads) != 0 {
		t.Errorf("expected no uploads, got %d", len(mock.uploads))
	}
}

func TestUpload_WritesUploadState(t *testing.T) {
	homeDir := t.TempDir()
	createPublicationSnapshots(t, homeDir, 1000)

	mock := newMockS3Uploader()
	uploader, err := newFixtureSnapshotUploader(t, homeDir, "my-bucket", "eu-central-1", "testchain", 0, mockUploaderFactory(mock))
	if err != nil {
		t.Fatalf("NewSnapshotUploader: %v", err)
	}

	_, err = uploader.Upload(context.Background())
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	state := uploader.readUploadState()
	if state.LastUploadedHeight != 1000 {
		t.Errorf("LastUploadedHeight = %d, want 1000", state.LastUploadedHeight)
	}
	if state.LastUploadedAt == 0 {
		t.Error("expected LastUploadedAt to be persisted on upload")
	}
}

// readTarGzNames decompresses + reads a tar.gz produced by writeArchive
// and returns the entry names + their typeflags.
func readTarGzNames(t *testing.T, body []byte) map[string]byte {
	t.Helper()
	gzr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer func() { _ = gzr.Close() }()
	tr := tar.NewReader(gzr)
	out := map[string]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		out[h.Name] = h.Typeflag
	}
	return out
}

func TestWriteArchive_OnlySelectedChunksAndIsolatedMetadata(t *testing.T) {
	home := t.TempDir()
	completed := createPublicationSnapshots(t, home, 1000, 2000)
	snapshotsDir := filepath.Join(home, "data", "snapshots")
	if err := os.WriteFile(filepath.Join(snapshotsDir, "metadata.db", "producer-only"), []byte("must not be copied"), 0o644); err != nil {
		t.Fatal(err)
	}
	metadataDir := t.TempDir()
	if err := writeSnapshotMetadata(metadataDir, *completed[0]); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- writeArchive(t.Context(), pw, snapshotsDir, metadataDir, *completed[0]) }()
	body, err := io.ReadAll(pr)
	_ = pr.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	names := readTarGzNames(t, body)
	if _, ok := names["1000/1/0"]; !ok {
		t.Fatalf("selected chunk absent: %v", names)
	}
	for name := range names {
		if strings.HasPrefix(name, "2000/") || name == "metadata.db/producer-only" {
			t.Fatalf("unselected producer data was archived: %s", name)
		}
	}
	restored := t.TempDir()
	if err := extractTarStream(t.Context(), bytes.NewReader(body), filepath.Join(restored, "data", "snapshots")); err != nil {
		t.Fatal(err)
	}
	if err := checkPublicationRestoresApplication(restored, 1000); err != nil {
		t.Fatal(err)
	}
}

// blockingUploader models a wedged S3 stream: UploadObject never returns until
// the context is cancelled, then surfaces the context error.
type blockingUploader struct{}

func (blockingUploader) UploadObject(ctx context.Context, _ *transfermanager.UploadObjectInput, _ ...func(*transfermanager.Options)) (*transfermanager.UploadObjectOutput, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func decodeUploadResult(t *testing.T, raw json.RawMessage) SnapshotUploadResult {
	t.Helper()
	var r SnapshotUploadResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("unmarshal result %q: %v", raw, err)
	}
	return r
}

func TestOnceHandler_ReturnsDistinguishableOutcomes(t *testing.T) {
	t.Run("uploaded", func(t *testing.T) {
		home := t.TempDir()
		createPublicationSnapshots(t, home, 1000)
		uploader, err := newFixtureSnapshotUploader(t, home, "b", "r", "once-uploaded", 0, mockUploaderFactory(newMockS3Uploader()))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := uploader.OnceHandler(time.Minute)(context.Background(), nil)
		if err != nil {
			t.Fatalf("handler error = %v", err)
		}
		got := decodeUploadResult(t, raw)
		if got.Outcome != OutcomeUploaded || got.Height != 1000 {
			t.Fatalf("result = %+v, want uploaded @1000", got)
		}
	})

	t.Run("noop no completed snapshots", func(t *testing.T) {
		home := t.TempDir()
		createPublicationSnapshots(t, home)
		uploader, err := newFixtureSnapshotUploader(t, home, "b", "r", "once-few", 0, mockUploaderFactory(newMockS3Uploader()))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := uploader.OnceHandler(time.Minute)(context.Background(), nil)
		if err != nil {
			t.Fatalf("handler error = %v", err)
		}
		got := decodeUploadResult(t, raw)
		if got.Outcome != OutcomeNoop || got.NoopReason != NoopNoCompletedSnapshots {
			t.Fatalf("result = %+v, want noop/no-completed-snapshots", got)
		}
	})

	t.Run("noop already uploaded", func(t *testing.T) {
		home := t.TempDir()
		createPublicationSnapshots(t, home, 1000)
		data, _ := json.Marshal(uploadState{LastUploadedHeight: 1000, ArchiveVerificationVersion: verifiedArchiveVersion, Bucket: "b", ChainID: "once-already"})
		if err := os.WriteFile(filepath.Join(home, uploadStateFile), data, 0o644); err != nil {
			t.Fatal(err)
		}
		uploader, err := newFixtureSnapshotUploader(t, home, "b", "r", "once-already", 0, mockUploaderFactory(newMockS3Uploader()))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := uploader.OnceHandler(time.Minute)(context.Background(), nil)
		if err != nil {
			t.Fatalf("handler error = %v", err)
		}
		got := decodeUploadResult(t, raw)
		if got.Outcome != OutcomeNoop || got.NoopReason != NoopAlreadyUploaded {
			t.Fatalf("result = %+v, want noop/already-uploaded", got)
		}
	})
}

// A handler-internal deadline must surface as context.DeadlineExceeded so the
// engine persists Failed rather than stranding the task in 'running'. Unlike a
// context.Canceled, a DeadlineExceeded is a genuine upload failure and must tick
// the error outcome counter — the recordError suppression keys only on Canceled.
func TestOnceHandler_DeadlineFailsCleanly(t *testing.T) {
	home := t.TempDir()
	createPublicationSnapshots(t, home, 1000)
	chain := "once-deadline"
	factory := func(context.Context, string) (seis3.Uploader, error) { return blockingUploader{}, nil }
	uploader, err := newFixtureSnapshotUploader(t, home, "b", "r", chain, 0, factory)
	if err != nil {
		t.Fatal(err)
	}

	_, err = uploader.OnceHandler(50*time.Millisecond)(context.Background(), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("handler error = %v, want context.DeadlineExceeded", err)
	}
	if got := testutil.ToFloat64(snapshotUploadOutcomes.WithLabelValues(chain, string(OutcomeError))); got != 1 {
		t.Errorf("deadline-exceeded is a genuine failure: error outcome counter = %v, want 1", got)
	}
}

// A canceled context — an operator DELETE or a node drain mid-upload — must not
// tick the error counter. It mirrors the engine's cancellation-suppression
// contract: a canceled task leaves no terminal record, so a drain is not counted
// as an upload failure. The result still carries OutcomeError, but the engine
// suppresses it on the canceled path so its value there is inert.
func TestUpload_CanceledDoesNotTickErrorCounter(t *testing.T) {
	home := t.TempDir()
	createPublicationSnapshots(t, home, 1000)
	chain := "cancel-no-tick"
	factory := func(context.Context, string) (seis3.Uploader, error) { return blockingUploader{}, nil }
	uploader, err := newFixtureSnapshotUploader(t, home, "b", "r", chain, 0, factory)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	result, err := uploader.Upload(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Upload error = %v, want context.Canceled", err)
	}
	if result.Outcome != OutcomeError {
		t.Errorf("result outcome = %q, want %q", result.Outcome, OutcomeError)
	}
	if got := testutil.ToFloat64(snapshotUploadOutcomes.WithLabelValues(chain, string(OutcomeError))); got != 0 {
		t.Errorf("canceled upload must not tick error counter, got %v", got)
	}
	if got := testutil.ToFloat64(snapshotUploadLastRunSuccess.WithLabelValues(chain)); got != 0 {
		t.Errorf("canceled path must not refresh last run success, got %v", got)
	}
}

// The loop must swallow per-iteration errors, keep running, and stop cleanly on
// context cancellation.
func TestRunLoop_SwallowsErrorsAndStopsOnCancel(t *testing.T) {
	home := t.TempDir()
	createPublicationSnapshots(t, home, 1000)
	var calls atomic.Int64
	factory := func(context.Context, string) (seis3.Uploader, error) {
		calls.Add(1)
		return nil, errors.New("s3 down")
	}
	uploader, err := newFixtureSnapshotUploader(t, home, "b", "r", "loop-chain", 5*time.Millisecond, factory)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- uploader.runLoop(ctx) }()

	time.Sleep(60 * time.Millisecond) // let it iterate and swallow several failures
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runLoop returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runLoop did not stop after cancel")
	}
	if calls.Load() < 2 {
		t.Fatalf("expected the loop to retry after swallowing errors, got %d attempts", calls.Load())
	}
}

// Concurrent writers exercise the temp-file + rename path: the persisted file
// must always parse to a complete written value, never a torn zero, and no temp
// files may be left behind.
func TestWriteUploadState_AtomicUnderConcurrentWriters(t *testing.T) {
	home := t.TempDir()
	uploader, err := newTestSnapshotUploader(home, "b", "r", "atomic-chain", 0, mockUploaderFactory(newMockS3Uploader()))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := int64(1); i <= 50; i++ {
		wg.Add(1)
		go func(h int64) {
			defer wg.Done()
			if err := uploader.writeUploadState(uploadState{LastUploadedHeight: h, LastUploadedAt: h}); err != nil {
				t.Errorf("writeUploadState(%d): %v", h, err)
			}
		}(i)
	}
	wg.Wait()

	st := uploader.readUploadState()
	if st.LastUploadedHeight < 1 || st.LastUploadedHeight > 50 {
		t.Fatalf("torn/lost write: height=%d, want a complete value in 1..50", st.LastUploadedHeight)
	}
	if st.LastUploadedAt != st.LastUploadedHeight {
		t.Fatalf("torn write: height=%d but at=%d (a single writer paired them)", st.LastUploadedHeight, st.LastUploadedAt)
	}

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file after atomic write: %s", e.Name())
		}
	}
}

// A pre-existing complete state survives a subsequent failed write attempt: the
// rename is all-or-nothing, so a torn temp never clobbers the live file.
func TestWriteUploadState_PreservesPriorStateOnRoundTrip(t *testing.T) {
	home := t.TempDir()
	uploader, err := newTestSnapshotUploader(home, "b", "r", "roundtrip-chain", 0, mockUploaderFactory(newMockS3Uploader()))
	if err != nil {
		t.Fatal(err)
	}
	if err := uploader.writeUploadState(uploadState{LastUploadedHeight: 4242, LastUploadedAt: 99}); err != nil {
		t.Fatal(err)
	}

	// A stray temp file (as a crashed write would leave) must be ignored by the
	// reader, which keys only on the canonical filename.
	if err := os.WriteFile(filepath.Join(home, uploadStateFile+".tmp-garbage"), []byte("{ torn"), 0o644); err != nil {
		t.Fatal(err)
	}

	st := uploader.readUploadState()
	if st.LastUploadedHeight != 4242 || st.LastUploadedAt != 99 {
		t.Fatalf("state = %+v, want height=4242 at=99", st)
	}
}

func TestUpload_EmitsMetrics(t *testing.T) {
	t.Run("upload sets all gauges", func(t *testing.T) {
		home := t.TempDir()
		createPublicationSnapshots(t, home, 1000)
		chain := "metrics-uploaded"
		uploader, err := newFixtureSnapshotUploader(t, home, "b", "r", chain, 0, mockUploaderFactory(newMockS3Uploader()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := uploader.Upload(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := testutil.ToFloat64(snapshotUploadOutcomes.WithLabelValues(chain, string(OutcomeUploaded))); got != 1 {
			t.Errorf("uploaded outcome counter = %v, want 1", got)
		}
		if got := testutil.ToFloat64(snapshotUploadLastUploadedHeight.WithLabelValues(chain)); got != 1000 {
			t.Errorf("last uploaded height = %v, want 1000", got)
		}
		if testutil.ToFloat64(snapshotUploadLastUploaded.WithLabelValues(chain)) == 0 {
			t.Error("last uploaded timestamp not set on upload")
		}
		if testutil.ToFloat64(snapshotUploadLastRunSuccess.WithLabelValues(chain)) == 0 {
			t.Error("last run success not set on upload")
		}
	})

	t.Run("noop refreshes success but not uploaded gauges", func(t *testing.T) {
		home := t.TempDir()
		createPublicationSnapshots(t, home) // no completed snapshots
		chain := "metrics-noop"
		uploader, err := newFixtureSnapshotUploader(t, home, "b", "r", chain, 0, mockUploaderFactory(newMockS3Uploader()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := uploader.Upload(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := testutil.ToFloat64(snapshotUploadOutcomes.WithLabelValues(chain, string(OutcomeNoop))); got != 1 {
			t.Errorf("noop outcome counter = %v, want 1", got)
		}
		if testutil.ToFloat64(snapshotUploadLastRunSuccess.WithLabelValues(chain)) == 0 {
			t.Error("noop must refresh last run success (a not-yet-advanced chain is healthy)")
		}
		if got := testutil.ToFloat64(snapshotUploadLastUploadedHeight.WithLabelValues(chain)); got != 0 {
			t.Errorf("noop must not advance uploaded height gauge, got %v", got)
		}
	})
}

// A failed Upload must return OutcomeError (so a poller reading the persisted
// result can tell failure apart from an empty outcome) and increment the
// chain-labeled outcome counter, while leaving the clean-terminal gauges
// untouched so a real S3 outage cannot read as green.
func TestUpload_ErrorOutcomeOnFailure(t *testing.T) {
	home := t.TempDir()
	createPublicationSnapshots(t, home, 1000)
	chain := "metrics-error"
	factory := func(context.Context, string) (seis3.Uploader, error) {
		return nil, errors.New("s3 down")
	}
	uploader, err := newFixtureSnapshotUploader(t, home, "b", "r", chain, 0, factory)
	if err != nil {
		t.Fatal(err)
	}

	result, err := uploader.Upload(context.Background())
	if err == nil {
		t.Fatal("expected Upload to error when the S3 uploader cannot be built")
	}
	if result.Outcome != OutcomeError {
		t.Errorf("result outcome = %q, want %q", result.Outcome, OutcomeError)
	}
	if got := testutil.ToFloat64(snapshotUploadOutcomes.WithLabelValues(chain, string(OutcomeError))); got != 1 {
		t.Errorf("error outcome counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(snapshotUploadLastRunSuccess.WithLabelValues(chain)); got != 0 {
		t.Errorf("error path must not refresh last run success, got %v", got)
	}
	if got := testutil.ToFloat64(snapshotUploadLastUploadedHeight.WithLabelValues(chain)); got != 0 {
		t.Errorf("error path must not advance uploaded height gauge, got %v", got)
	}
}

func TestEmitStartupMetrics_RehydratesUploadedGauges(t *testing.T) {
	home := t.TempDir()
	chain := "startup-chain"
	uploader, err := newTestSnapshotUploader(home, "b", "r", chain, 0, mockUploaderFactory(newMockS3Uploader()))
	if err != nil {
		t.Fatal(err)
	}
	if err := uploader.writeUploadState(uploadState{LastUploadedHeight: 7777, LastUploadedAt: 1234567890, ArchiveVerificationVersion: verifiedArchiveVersion, Bucket: "b", ChainID: chain}); err != nil {
		t.Fatal(err)
	}

	uploader.EmitStartupMetrics()

	if got := testutil.ToFloat64(snapshotUploadLastUploadedHeight.WithLabelValues(chain)); got != 7777 {
		t.Errorf("startup height gauge = %v, want 7777", got)
	}
	if got := testutil.ToFloat64(snapshotUploadLastUploaded.WithLabelValues(chain)); got != 1234567890 {
		t.Errorf("startup uploaded timestamp = %v, want 1234567890", got)
	}
	// The alert signal must stay unset on startup so a genuinely stalled uploader
	// is not masked by a persisted success timestamp.
	if got := testutil.ToFloat64(snapshotUploadLastRunSuccess.WithLabelValues(chain)); got != 0 {
		t.Errorf("startup must not set last run success, got %v", got)
	}
}

func newTestSnapshotUploader(home, bucket, region, chain string, interval time.Duration, factory seis3.UploaderFactory) (*SnapshotUploader, error) {
	markerFactory := func(ctx context.Context, region string) (seis3.LatestMarker, error) {
		client, err := factory(ctx, region)
		if err != nil {
			return nil, err
		}
		return seis3.NewLatestMarker(uploadTestMarkerClient{client}), nil
	}
	return NewSnapshotUploader(home, bucket, region, chain, interval, factory, markerFactory)
}

// These existing tests have one publisher. Preserve their UploadObject fault
// boundary; the independent publisher tests use atomic S3 conditions over HTTP.
type uploadTestMarkerClient struct{ uploader seis3.Uploader }

func (uploadTestMarkerClient) GetObject(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	return nil, &smithy.GenericAPIError{Code: "NoSuchKey"}
}

func (c uploadTestMarkerClient) PutObject(ctx context.Context, input *awss3.PutObjectInput, _ ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
	if input.IfNoneMatch == nil || *input.IfNoneMatch != "*" {
		return nil, fmt.Errorf("missing create condition")
	}
	_, err := c.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{Bucket: input.Bucket, Key: input.Key, Body: input.Body})
	return &awss3.PutObjectOutput{}, err
}
