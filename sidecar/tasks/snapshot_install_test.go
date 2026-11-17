package tasks

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
)

const restoreNeighborCheckpoint = `{"lastUploadedHeight":2000,"lastUploadedAt":123,"pendingHeight":3000,"pendingBucket":"pending-bucket","pendingChainID":"pending-chain"}`

func TestSnapshotRestoreFailedExtractionPreservesRecordsAndAllowsRetry(t *testing.T) {
	for _, priorHeight := range []string{"", "previous height"} {
		t.Run("prior height="+priorHeight, func(t *testing.T) {
			home := t.TempDir()
			writeRestoreNeighbors(t, home)
			if priorHeight != "" {
				writeRestoreTestFile(t, filepath.Join(home, SnapshotHeightFile), priorHeight)
			}
			archive := buildTarGzArchive(t, map[string]string{"chunk": "complete chunk"})
			client := &mockTransferClient{responses: map[string][]byte{"chain/state-sync/1000.tar.gz": archive}}
			restorer := diagnosticRestorer(t, home, map[string][]byte{"1000": truncatedRestoreChunkArchive(t)})
			before := captureRestoreTree(t, home)
			if err := restorer.Restore(t.Context(), 1000); err == nil {
				t.Fatal("invalid archive succeeded")
			}
			if !reflect.DeepEqual(before, captureRestoreTree(t, home)) {
				t.Fatal("failed extraction changed existing files")
			}
			requireNoRestoreStaging(t, home)
			if _, err := os.Stat(filepath.Join(home, "data", "snapshots")); !os.IsNotExist(err) {
				t.Fatalf("failed extraction installed a destination: %v", err)
			}

			restorer.clientFactory = mockClientFactory(client)
			if err := restorer.Restore(t.Context(), 1000); err != nil {
				t.Fatalf("valid retry: %v", err)
			}
			requireRestoreNeighbors(t, home)
			requireNoRestoreStaging(t, home)
		})
	}
}

func TestSnapshotRestoreInstallsIntoEmptyDirectoryAndIgnoresInterruptedStaging(t *testing.T) {
	home := t.TempDir()
	writeRestoreNeighbors(t, home)
	destination := filepath.Join(home, "data", "snapshots")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	abandoned := filepath.Join(home, "data", snapshotStagingPrefix+"interrupted", "metadata.db", "stale.log")
	writeRestoreTestFile(t, abandoned, "partial foreign journal")
	archive := createRestoreDiagnosticArchive(t, 1000, 0)
	restorer := diagnosticRestorer(t, home, map[string][]byte{"1000": diagnosticChunksFirstArchive(t, archive.directory)})
	if err := restorer.Restore(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	requireDiagnosticSnapshotMembership(t, home, 1000)
	requireRestoreNeighbors(t, home)
	contents, err := os.ReadFile(abandoned)
	if err != nil || string(contents) != "partial foreign journal" {
		t.Fatalf("foreign staging was changed: %q, %v", contents, err)
	}
}

func TestSnapshotRestoreRejectsDestinationInstalledDuringDownload(t *testing.T) {
	home := t.TempDir()
	writeRestoreNeighbors(t, home)
	previous := createRestoreDiagnosticArchive(t, 2000, 3)
	incoming := createRestoreDiagnosticArchive(t, 1000, 0)
	destination := filepath.Join(home, "data", "snapshots")
	var before map[string]string
	client := &restoreHookClient{
		mockTransferClient: &mockTransferClient{responses: map[string][]byte{"chain/state-sync/1000.tar.gz": diagnosticChunksFirstArchive(t, incoming.directory)}},
		afterDownload: func() {
			if err := os.Rename(previous.directory, destination); err != nil {
				t.Fatal(err)
			}
			before = captureRestoreTree(t, destination)
		},
	}
	restorer := mustNewRestorer(t, home, "bucket", "region", "chain", mockClientFactory(client), mockListerFactory(&mockObjectLister{keys: []string{"chain/state-sync/1000.tar.gz"}}))
	if err := restorer.Restore(t.Context(), 1000); err == nil || !strings.Contains(err.Error(), "is not empty") {
		t.Fatalf("competing installation was not rejected: %v", err)
	}
	if before == nil || !reflect.DeepEqual(before, captureRestoreTree(t, destination)) {
		t.Fatal("restore changed the competing SDK store")
	}
	if markerExists(home, restoreMarkerFile) || markerExists(home, SnapshotHeightFile) {
		t.Fatal("rejected installation published restore records")
	}
	requireDiagnosticSnapshotMembership(t, home, 2000)
	requireRestoreNeighbors(t, home)
	requireNoRestoreStaging(t, home)
}

func TestSnapshotRestoreCancelledBeforeInstallationAllowsRetry(t *testing.T) {
	home := t.TempDir()
	writeRestoreNeighbors(t, home)
	archive := createRestoreDiagnosticArchive(t, 1000, 0)
	body := diagnosticChunksFirstArchive(t, archive.directory)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &restoreHookClient{mockTransferClient: &mockTransferClient{responses: map[string][]byte{"chain/state-sync/1000.tar.gz": body}}, afterDownload: cancel}
	restorer := mustNewRestorer(t, home, "bucket", "region", "chain", mockClientFactory(client), mockListerFactory(&mockObjectLister{keys: []string{"chain/state-sync/1000.tar.gz"}}))
	before := captureRestoreTree(t, home)
	if err := restorer.Restore(ctx, 1000); err == nil {
		t.Fatal("cancelled restore succeeded")
	}
	if !reflect.DeepEqual(before, captureRestoreTree(t, home)) {
		t.Fatal("cancelled restore changed existing files")
	}
	requireNoRestoreStaging(t, home)
	client.afterDownload = nil
	if err := restorer.Restore(t.Context(), 1000); err != nil {
		t.Fatalf("retry after cancellation: %v", err)
	}
	requireDiagnosticSnapshotMembership(t, home, 1000)
	requireRestoreNeighbors(t, home)
}

func TestSnapshotRestoreHeightRecordFailurePreservesInstalledStoreAndRejectsRetry(t *testing.T) {
	home := t.TempDir()
	writeRestoreNeighbors(t, home)
	// A directory at the record path deterministically prevents record publication.
	if err := os.Mkdir(filepath.Join(home, SnapshotHeightFile), 0o755); err != nil {
		t.Fatal(err)
	}
	incoming := createRestoreDiagnosticArchive(t, 1000, 0)
	restorer := diagnosticRestorer(t, home, map[string][]byte{"1000": diagnosticChunksFirstArchive(t, incoming.directory)})
	if err := restorer.Restore(t.Context(), 1000); err == nil || !strings.Contains(err.Error(), "snapshot installed but height recording failed") {
		t.Fatalf("height failure must report installed state: %v", err)
	}
	if markerExists(home, restoreMarkerFile) {
		t.Fatal("failed height recording published completion")
	}
	requireNoRestoreStaging(t, home)
	before := captureRestoreTree(t, home)
	if err := restorer.Restore(t.Context(), 1000); err == nil || !strings.Contains(err.Error(), "is not empty") {
		t.Fatalf("retry must explicitly refuse the preserved installation: %v", err)
	}
	if !reflect.DeepEqual(before, captureRestoreTree(t, home)) {
		t.Fatal("retry modified the preserved installation")
	}
	requireDiagnosticSnapshotMembership(t, home, 1000)
	requireRestoreNeighbors(t, home)
}

func TestSnapshotRestoreCompletionRecordFailurePreservesInstalledStoreAndRejectsRetry(t *testing.T) {
	home := t.TempDir()
	writeRestoreNeighbors(t, home)
	incoming := createRestoreDiagnosticArchive(t, 1000, 0)
	completionPath := filepath.Join(home, restoreMarkerFile)
	client := &restoreHookClient{
		mockTransferClient: &mockTransferClient{responses: map[string][]byte{"chain/state-sync/1000.tar.gz": diagnosticChunksFirstArchive(t, incoming.directory)}},
		afterDownload: func() {
			// Introduce the failure after Restore checks its completion record.
			if err := os.Mkdir(completionPath, 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
	restorer := mustNewRestorer(t, home, "bucket", "region", "chain", mockClientFactory(client), mockListerFactory(&mockObjectLister{keys: []string{"chain/state-sync/1000.tar.gz"}}))
	if err := restorer.Restore(t.Context(), 1000); err == nil || !strings.Contains(err.Error(), "snapshot installed but completion recording failed") {
		t.Fatalf("completion failure must report installed state: %v", err)
	}
	info, err := os.Lstat(completionPath)
	if err != nil || !info.IsDir() {
		t.Fatalf("restore replaced the conflicting directory with a completion record: %v", err)
	}
	height, err := os.ReadFile(filepath.Join(home, SnapshotHeightFile))
	if err != nil || string(height) != "1000" {
		t.Fatalf("installed height must be recorded: %q, %v", height, err)
	}
	before := captureRestoreTree(t, home)
	if err := restorer.Restore(t.Context(), 1000); err == nil || !strings.Contains(err.Error(), "must be a regular file") {
		t.Fatalf("invalid completion record must not skip retry: %v", err)
	}
	if !reflect.DeepEqual(before, captureRestoreTree(t, home)) {
		t.Fatal("retry changed preserved installation")
	}
	requireDiagnosticSnapshotMembership(t, home, 1000)
	requireRestoreNeighbors(t, home)
	requireNoRestoreStaging(t, home)
	records, err := filepath.Glob(filepath.Join(home, ".snapshot-record-*"))
	if err != nil || len(records) != 0 {
		t.Fatalf("failed completion left temporary records: %v, %v", records, err)
	}
}

func TestSnapshotRestoreRejectsFileOrSymlinkDestination(t *testing.T) {
	for _, kind := range []string{"file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			writeRestoreNeighbors(t, home)
			destination := filepath.Join(home, "data", "snapshots")
			if kind == "file" {
				writeRestoreTestFile(t, destination, "existing data")
			} else if err := os.Symlink(".sei-sidecar-last-upload.json", destination); err != nil {
				t.Fatal(err)
			}
			before := captureRestoreTree(t, home)
			restorer := diagnosticRestorer(t, home, nil)
			if err := restorer.Restore(t.Context(), 1000); err == nil || !strings.Contains(err.Error(), "must be an absent or empty directory") {
				t.Fatalf("destination must be explicitly rejected: %v", err)
			}
			if !reflect.DeepEqual(before, captureRestoreTree(t, home)) {
				t.Fatal("rejected destination changed existing files")
			}
		})
	}
}

type restoreHookClient struct {
	*mockTransferClient
	afterDownload func()
}

func (c *restoreHookClient) DownloadObject(ctx context.Context, in *transfermanager.DownloadObjectInput, options ...func(*transfermanager.Options)) (*transfermanager.DownloadObjectOutput, error) {
	result, err := c.mockTransferClient.DownloadObject(ctx, in, options...)
	if err == nil && c.afterDownload != nil {
		c.afterDownload()
	}
	return result, err
}

func writeRestoreTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeRestoreNeighbors(t *testing.T, home string) {
	t.Helper()
	writeRestoreTestFile(t, filepath.Join(home, uploadStateFile), restoreNeighborCheckpoint)
	writeRestoreTestFile(t, filepath.Join(home, "data", ".sei-sidecar-last-upload.json"), "unrelated data neighbor")
	writeRestoreTestFile(t, filepath.Join(home, "data", "application.db", "state"), "unrelated application data")
	writeRestoreTestFile(t, filepath.Join(home, "config", "config.toml"), "unrelated config")
}

func requireRestoreNeighbors(t *testing.T, home string) {
	t.Helper()
	for path, want := range map[string]string{
		uploadStateFile:                      restoreNeighborCheckpoint,
		"data/.sei-sidecar-last-upload.json": "unrelated data neighbor",
		"data/application.db/state":          "unrelated application data",
		"config/config.toml":                 "unrelated config",
	} {
		contents, err := os.ReadFile(filepath.Join(home, path))
		if err != nil || string(contents) != want {
			t.Fatalf("restore changed %s: %q, %v", path, contents, err)
		}
	}
}

// captureRestoreTree compares file contents, modes and symlink targets before SDK reopen.
func captureRestoreTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		var contents []byte
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			contents = []byte(target)
		} else {
			contents, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		files[name] = info.Mode().String() + ":" + string(contents)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func requireNoRestoreStaging(t *testing.T, home string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(home, "data", snapshotStagingPrefix+"*"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("restore left staging directories: %v, %v", paths, err)
	}
}

// The chunk is complete, but the stream stops before metadata and tar completion.
func truncatedRestoreChunkArchive(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	gzipWriter := gzip.NewWriter(&body)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "1000/1/0", Mode: 0o600, Size: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte("chunk")); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Flush(); err != nil {
		t.Fatal(err)
	}
	return bytes.Clone(body.Bytes())
}
