package tasks

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/prometheus/client_golang/prometheus/testutil"

	seis3 "github.com/sei-protocol/sei-k8s-controller/sidecar/s3"
)

// The old uploader could report success for an incomplete archive. Its checkpoint
// must not suppress a verified replacement at the same or a lower completed height.
func TestSnapshotPublication_UpgradeIgnoresLegacyCheckpoint(t *testing.T) {
	for _, legacyHeight := range []int64{1000, 2000} {
		t.Run(fmt.Sprintf("legacy height %d", legacyHeight), func(t *testing.T) {
			home := t.TempDir()
			createPublicationSnapshots(t, home, 1000)
			legacy := fmt.Appendf(nil, "{\"lastUploadedHeight\":%d,\"lastUploadedAt\":123}", legacyHeight)
			if err := os.WriteFile(filepath.Join(home, uploadStateFile), legacy, 0o644); err != nil {
				t.Fatal(err)
			}
			bucket := newPublicationReproBucket(t)
			incomplete := buildTarGzArchive(t, map[string]string{
				fmt.Sprintf("%d/1/0", legacyHeight): "interrupted chunk",
			})
			prior := t.TempDir()
			if err := extractTarStream(t.Context(), bytes.NewReader(incomplete), filepath.Join(prior, "data", "snapshots")); err != nil {
				t.Fatalf("legacy fixture must extract successfully: %v", err)
			}
			if err := checkPublicationRestoresApplication(prior, uint64(legacyHeight)); err == nil {
				t.Fatal("legacy fixture unexpectedly contains a restorable application snapshot")
			}
			client := bucket.client()
			if _, err := client.PutObject(t.Context(), &awss3.PutObjectInput{
				Bucket: aws.String("bucket"), Key: aws.String(fmt.Sprintf("chain/state-sync/%d.tar.gz", legacyHeight)),
				Body: bytes.NewReader(incomplete),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := seis3.NewLatestMarker(client).Advance(t.Context(), "bucket", "chain/state-sync/latest.txt", legacyHeight); err != nil {
				t.Fatal(err)
			}

			uploader, err := newFixtureSnapshotUploader(t, home, "bucket", "eu-central-1", "chain", 0, bucket.uploaderFactory)
			if err != nil {
				t.Fatal(err)
			}
			uploader.latestMarkerFactory = bucket.markerFactory
			result, err := uploader.Upload(t.Context())
			if err != nil || result.Outcome != OutcomeUploaded || result.Height != 1000 {
				t.Fatalf("upgrade = %+v, %v; want verified publication of completed 1000", result, err)
			}
			requireCheckpointRegressionArchiveRestores(t, bucket, 1000)
			requirePublicationReproLatest(t, bucket, fmt.Sprint(legacyHeight))
			if legacyHeight > 1000 {
				// Publishing a valid lower archive cannot repair an unrelated bad
				// higher object. Recovery must inspect it without lowering the marker.
				if result.MarkerOutcome != seis3.MarkerNewer || !bytes.Equal(bucket.object("2000.tar.gz"), incomplete) {
					t.Fatal("upgrade claimed or changed the unverified higher publication")
				}
				key, err := resolveKeyForHeight(t.Context(), &mockObjectLister{keys: []string{"chain/state-sync/1000.tar.gz", "chain/state-sync/2000.tar.gz"}}, "bucket", "chain/state-sync/", "eu-central-1", 0)
				if err != nil || key != "chain/state-sync/2000.tar.gz" {
					t.Fatalf("default restore discovery = %q, %v; want the unchanged higher archive", key, err)
				}
			}
			// A restarted uploader may trust only the new verified cache.
			uploader, err = newFixtureSnapshotUploader(t, home, "bucket", "eu-central-1", "chain", 0, bucket.uploaderFactory)
			if err != nil {
				t.Fatal(err)
			}
			uploader.latestMarkerFactory = bucket.markerFactory
			again, err := uploader.Upload(t.Context())
			if err != nil || again.Outcome != OutcomeNoop || again.NoopReason != NoopAlreadyUploaded {
				t.Fatalf("verified retry = %+v, %v; want the new successful checkpoint to suppress a duplicate", again, err)
			}
		})
	}
}

// An empty producer inventory remains a healthy no-op. Its historical checkpoint
// stays unchanged for audit, without asserting that any remote object is corrupt.
func TestSnapshotPublication_LegacyCheckpointWithoutCompletedSnapshotStaysNoop(t *testing.T) {
	home := t.TempDir()
	legacy := []byte("{\"lastUploadedHeight\":2000,\"lastUploadedAt\":123}")
	if err := os.WriteFile(filepath.Join(home, uploadStateFile), legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	factory := func(context.Context, string) (seis3.Uploader, error) {
		t.Error("attempted S3 access without a completed replacement")
		return nil, fmt.Errorf("unexpected S3 access")
	}
	uploader, err := newFixtureSnapshotUploader(t, home, "bucket", "region", "legacy-empty", 0, factory)
	if err != nil {
		t.Fatal(err)
	}
	result, err := uploader.Upload(t.Context())
	if err != nil || result.Outcome != OutcomeNoop || result.NoopReason != NoopNoCompletedSnapshots {
		t.Fatalf("empty producer = %+v, %v; want successful no-completed-snapshots outcome", result, err)
	}
	after, err := os.ReadFile(filepath.Join(home, uploadStateFile))
	if err != nil || !bytes.Equal(after, legacy) {
		t.Fatalf("legacy checkpoint changed without verification: %q, %v", after, err)
	}
}

func TestSnapshotPublication_LegacyCheckpointDoesNotEmitVerifiedMetrics(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, uploadStateFile), []byte("{\"lastUploadedHeight\":2000,\"lastUploadedAt\":123}"), 0o644); err != nil {
		t.Fatal(err)
	}
	uploader, err := NewSnapshotUploader(home, "bucket", "region", "legacy-startup", 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	uploader.EmitStartupMetrics()
	if got := testutil.ToFloat64(snapshotUploadLastUploadedHeight.WithLabelValues("legacy-startup")); got != 0 {
		t.Fatalf("verified uploaded height = %v; want no verified success from a legacy checkpoint", got)
	}
	if got := testutil.ToFloat64(snapshotUploadLastUploaded.WithLabelValues("legacy-startup")); got != 0 {
		t.Fatalf("verified upload timestamp = %v; want no legacy success timestamp", got)
	}
}
