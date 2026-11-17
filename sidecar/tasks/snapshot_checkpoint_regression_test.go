package tasks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	snapshottypes "github.com/sei-protocol/sei-chain/sei-cosmos/snapshots/types"

	seis3 "github.com/sei-protocol/sei-k8s-controller/sidecar/s3"
)

// A failed local checkpoint leaves the next attempt free to archive an older
// completed snapshot. The shared marker must retain the newer accepted height.
func TestSnapshotPublication_CheckpointFailureDoesNotRegressLatest(t *testing.T) {
	for _, test := range []struct {
		name        string
		reconstruct bool
	}{
		{name: "same uploader"},
		{name: "reconstructed uploader", reconstruct: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			completed := createPublicationSnapshots(t, home, 1000, 2000)
			bucket := newPublicationReproBucket(t)
			outage := &checkpointDirectoryOutageMarker{
				LatestMarker: seis3.NewLatestMarker(bucket.client()),
				home:         home,
				movedHome:    home + "-checkpoint-unavailable",
			}
			t.Cleanup(func() {
				if _, err := os.Stat(outage.movedHome); err == nil {
					if err := os.Rename(outage.movedHome, home); err != nil {
						t.Errorf("restoring checkpoint directory during cleanup: %v", err)
					}
				}
			})
			markerFactory := func(context.Context, string) (seis3.LatestMarker, error) {
				return outage, nil
			}
			uploader, err := NewSnapshotUploader(home, "bucket", "eu-central-1", "chain", 0, bucket.uploaderFactory, markerFactory)
			if err != nil {
				t.Fatal(err)
			}
			uploader.snapshotClient = snapshotListFunc(func(context.Context) ([]snapshottypes.Snapshot, error) {
				return []snapshottypes.Snapshot{*completed[0], *completed[1]}, nil
			})

			// The marker wrapper removes the home only after the real SDK accepts
			// 2000. The ensuing filesystem failure belongs to the final save.
			failed, err := uploader.Upload(t.Context())
			if !outage.interrupted || !errors.Is(err, os.ErrNotExist) || failed.Outcome != OutcomeError {
				t.Fatalf("first upload=%+v error=%v outage=%v; want final checkpoint failure", failed, err, outage.interrupted)
			}
			requirePublicationReproLatest(t, bucket, "2000")
			if err := os.Rename(outage.movedHome, home); err != nil {
				t.Fatal(err)
			}
			if state := uploader.readUploadState(); state.LastUploadedHeight != 0 {
				t.Fatalf("failed save left local checkpoint %+v; want no successful upload", state)
			}
			if _, err := os.Stat(filepath.Join(home, uploadStateFile)); !os.IsNotExist(err) {
				t.Fatalf("checkpoint after failed save: %v; want no checkpoint file", err)
			}

			if test.reconstruct {
				uploader, err = NewSnapshotUploader(home, "bucket", "eu-central-1", "chain", 0, bucket.uploaderFactory, bucket.markerFactory)
				if err != nil {
					t.Fatal(err)
				}
			}
			queries := 0
			uploader.snapshotClient = snapshotListFunc(func(context.Context) ([]snapshottypes.Snapshot, error) {
				queries++
				return []snapshottypes.Snapshot{*completed[0]}, nil
			})

			result, err := uploader.Upload(t.Context())
			requirePublicationReproLatest(t, bucket, "2000")
			if err != nil || result.Outcome != OutcomeUploaded || result.Height != 1000 || result.MarkerOutcome != seis3.MarkerNewer {
				t.Fatalf("follow-up=%+v error=%v; want archived 1000 with newer marker retained", result, err)
			}
			if queries != 1 {
				t.Fatalf("producer queries=%d; want the ordinary older-snapshot attempt", queries)
			}
			// Local success describes this node's archive, not the remote floor.
			if state := uploader.readUploadState(); state.LastUploadedHeight != 1000 {
				t.Fatalf("local checkpoint=%+v; want successfully archived 1000", state)
			}
			requireCheckpointRegressionArchiveRestores(t, bucket, 1000)
			requireCheckpointRegressionArchiveRestores(t, bucket, 2000)
		})
	}
}

// checkpointDirectoryOutageMarker makes the home unavailable after publication.
type checkpointDirectoryOutageMarker struct {
	seis3.LatestMarker
	home        string
	movedHome   string
	interrupted bool
}

func (m *checkpointDirectoryOutageMarker) Advance(ctx context.Context, bucket, key string, height int64) (seis3.MarkerOutcome, error) {
	outcome, err := m.LatestMarker.Advance(ctx, bucket, key, height)
	if err != nil || m.interrupted {
		return outcome, err
	}
	if err := os.Rename(m.home, m.movedHome); err != nil {
		return "", fmt.Errorf("injecting checkpoint directory outage: %w", err)
	}
	m.interrupted = true
	return outcome, nil
}

type snapshotListFunc func(context.Context) ([]snapshottypes.Snapshot, error)

func (f snapshotListFunc) ListSnapshots(ctx context.Context) ([]snapshottypes.Snapshot, error) {
	return f(ctx)
}

// requireCheckpointRegressionArchiveRestores verifies the original producer's
// application data using the bytes uploaded through the real AWS SDK.
func requireCheckpointRegressionArchiveRestores(t *testing.T, bucket *publicationReproBucket, height uint64) {
	t.Helper()
	archive := bucket.object(fmt.Sprintf("%d.tar.gz", height))
	if len(archive) == 0 {
		t.Fatalf("snapshot %d was not uploaded", height)
	}
	restored := t.TempDir()
	if err := extractTarStream(t.Context(), bytes.NewReader(archive), filepath.Join(restored, "data", "snapshots")); err != nil {
		t.Fatalf("extract snapshot %d: %v", height, err)
	}
	if err := checkPublicationRestoresApplication(restored, height); err != nil {
		t.Fatalf("snapshot %d cannot restore the application: %v", height, err)
	}
}
