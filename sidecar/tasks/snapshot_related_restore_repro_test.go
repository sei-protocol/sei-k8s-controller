package tasks

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/sei-protocol/sei-chain/sei-cosmos/snapshots"
	snapshottypes "github.com/sei-protocol/sei-chain/sei-cosmos/snapshots/types"
	"github.com/syndtr/goleveldb/leveldb/journal"
	db "github.com/tendermint/tm-db"
)

// An isolated archive must remain readable after installation into a new home.
func TestRelatedRestore_EmptyDestinationContainsOnlyArchivedSnapshot(t *testing.T) {
	archive := createRestoreDiagnosticArchive(t, 1000, 0)
	home := t.TempDir()
	restorer := diagnosticRestorer(t, home, map[string][]byte{"1000": diagnosticChunksFirstArchive(t, archive.directory)})

	if err := restorer.Restore(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}

	requireDiagnosticSnapshotMembership(t, home, 1000)
}

// A node can already have a closed snapshot store without a restore marker.
// Reject it before modifying any existing files or publishing restore records.
func TestRelatedRestore_ExistingStoreDoesNotOverrideRestoredSnapshot(t *testing.T) {
	incoming := createRestoreDiagnosticArchive(t, 1000, 0)
	previous := createRestoreDiagnosticArchive(t, 2000, 3)
	if previous.journalNumber <= incoming.journalNumber || previous.batchSequence <= incoming.batchSequence {
		t.Fatalf("fixture: previous journal must exceed incoming recovery history: previous=%s sequence %d incoming=%s sequence %d", previous.journalName, previous.batchSequence, incoming.journalName, incoming.batchSequence)
	}

	home := t.TempDir()
	destination := filepath.Join(home, "data", "snapshots")
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(previous.directory, destination); err != nil {
		t.Fatal(err)
	}
	if markerExists(home, restoreMarkerFile) {
		t.Fatal("fixture: an ordinary producer store must have no restore marker")
	}
	previousChunk := filepath.Join(destination, "2000", strconv.FormatUint(uint64(snapshottypes.CurrentFormat), 10), "0")
	if _, err := os.Stat(previousChunk); err != nil {
		t.Fatalf("fixture: previous completed snapshot chunk is absent: %v", err)
	}
	t.Logf("existing closed SDK store: height 2000 chunk present, journal=%s sequence=%d; incoming journal=%s sequence=%d", previous.journalName, previous.batchSequence, incoming.journalName, incoming.batchSequence)

	// Preserve ordinary writer ordering: selected chunks, then metadata.
	writeRestoreNeighbors(t, home)
	before := captureRestoreTree(t, home)
	body := diagnosticChunksFirstArchive(t, incoming.directory)
	restorer := diagnosticRestorer(t, home, map[string][]byte{"1000": body})
	err := restorer.Restore(t.Context(), 1000)
	if err == nil || !strings.Contains(err.Error(), "is not empty") {
		t.Fatalf("existing store must be explicitly rejected, got %v", err)
	}
	if after := captureRestoreTree(t, home); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected restore changed existing data or restore records")
	}
	// Reopen only after comparing bytes: LevelDB reopening can change its files.
	requireDiagnosticSnapshotMembership(t, home, 2000)
}

// This truncation puts metadata first; current uploads put chunks first.
// Failed extraction must remain private, and a valid retry must install one store.
func TestRelatedRestore_RetryDoesNotReplayForeignMetadataJournal(t *testing.T) {
	incoming := createRestoreDiagnosticArchive(t, 1000, 0)
	interrupted := createRestoreDiagnosticArchive(t, 2000, 3)
	home := t.TempDir()
	writeRestoreNeighbors(t, home)
	writeRestoreTestFile(t, filepath.Join(home, SnapshotHeightFile), "prior height")
	before := captureRestoreTree(t, home)
	restorer := diagnosticRestorer(t, home, map[string][]byte{
		"1000": diagnosticChunksFirstArchive(t, incoming.directory),
		"2000": interrupted.throughMetadata,
	})
	if err := restorer.Restore(t.Context(), 2000); err == nil {
		t.Fatal("restore of the truncated archive unexpectedly succeeded")
	}
	if after := captureRestoreTree(t, home); !reflect.DeepEqual(before, after) {
		t.Fatal("failed extraction changed existing data or restore records")
	}
	if _, err := os.Stat(filepath.Join(home, "data", "snapshots")); !os.IsNotExist(err) {
		t.Fatalf("failed extraction must not install a partial store: %v", err)
	}
	requireNoRestoreStaging(t, home)

	if err := restorer.Restore(t.Context(), 1000); err != nil {
		t.Fatalf("valid retry failed: %v", err)
	}
	requireDiagnosticSnapshotMembership(t, home, 1000)
	requireRestoreNeighbors(t, home)
	requireNoRestoreStaging(t, home)
	height, err := os.ReadFile(filepath.Join(home, SnapshotHeightFile))
	if err != nil || string(height) != "1000" || !markerExists(home, restoreMarkerFile) {
		t.Fatalf("successful retry must record installed height and completion: height=%q error=%v", height, err)
	}
}

type restoreDiagnosticArchive struct {
	directory       string
	complete        []byte
	throughMetadata []byte
	journalName     string
	journalNumber   uint64
	batchSequence   uint64
}

// createRestoreDiagnosticArchive uses SDK Save/Delete and ordinary database reopenings.
// Its truncated prefix ends after complete metadata files and before any chunk.
func createRestoreDiagnosticArchive(t *testing.T, height uint64, reopenings int) restoreDiagnosticArchive {
	t.Helper()
	dir := t.TempDir()
	for generation := range reopenings {
		metadata, err := db.NewGoLevelDB("metadata", dir)
		if err != nil {
			t.Fatal(err)
		}
		store, err := snapshots.NewStore(metadata, dir)
		if err != nil {
			t.Fatal(err)
		}
		// Real snapshot creation and pruning advance both LevelDB sequence
		// numbers and journal history without leaving extra completion records.
		priorHeight := height + uint64(generation) + 1
		saveDiagnosticStoreSnapshot(t, store, priorHeight)
		if err := store.Delete(priorHeight, snapshottypes.CurrentFormat); err != nil {
			t.Fatal(err)
		}
		if err := metadata.Close(); err != nil {
			t.Fatal(err)
		}
	}
	metadata, err := db.NewGoLevelDB("metadata", dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := snapshots.NewStore(metadata, dir)
	if err != nil {
		t.Fatal(err)
	}
	saveDiagnosticStoreSnapshot(t, store, height)
	listed, err := store.List()
	if err != nil || len(listed) != 1 || listed[0].Height != height {
		t.Fatalf("fixture: producer metadata is not isolated to height %d: list=%v error=%v", height, listed, err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}

	archive := restoreDiagnosticArchive{directory: dir}
	var body bytes.Buffer
	gzipWriter := gzip.NewWriter(&body)
	tarWriter := tar.NewWriter(gzipWriter)
	metadataDir := filepath.Join(dir, "metadata.db")
	entries, err := os.ReadDir(metadataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if stem, ok := strings.CutSuffix(entry.Name(), ".log"); ok {
			number, err := strconv.ParseUint(stem, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			if number > archive.journalNumber {
				archive.journalNumber, archive.journalName = number, entry.Name()
			}
		}
		appendDiagnosticArchiveFile(t, tarWriter, dir, filepath.Join("metadata.db", entry.Name()))
	}
	if archive.journalName == "" {
		t.Fatal("fixture: real LevelDB produced no journal")
	}
	archive.batchSequence = diagnosticJournalSequence(t, filepath.Join(metadataDir, archive.journalName))
	if err := tarWriter.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Flush(); err != nil {
		t.Fatal(err)
	}
	archive.throughMetadata = bytes.Clone(body.Bytes())

	chunkName := filepath.Join(strconv.FormatUint(height, 10), strconv.FormatUint(uint64(snapshottypes.CurrentFormat), 10), "0")
	appendDiagnosticArchiveFile(t, tarWriter, dir, chunkName)
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	archive.complete = bytes.Clone(body.Bytes())
	return archive
}

// diagnosticChunksFirstArchive preserves uploader ordering: chunks before metadata.
// It includes exactly the isolated snapshot in the source directory.
func diagnosticChunksFirstArchive(t *testing.T, dir string) []byte {
	t.Helper()
	var body bytes.Buffer
	gzipWriter := gzip.NewWriter(&body)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		name, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		appendDiagnosticArchiveFile(t, tarWriter, dir, name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func appendDiagnosticArchiveFile(t *testing.T, writer *tar.Writer, dir, name string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: filepath.ToSlash(name), Mode: 0o600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
}

func diagnosticRestorer(t *testing.T, home string, byHeight map[string][]byte) *SnapshotRestorer {
	t.Helper()
	responses := make(map[string][]byte, len(byHeight))
	keys := make([]string, 0, len(byHeight))
	for height, body := range byHeight {
		key := "chain/state-sync/" + height + ".tar.gz"
		responses[key] = body
		keys = append(keys, key)
	}
	return mustNewRestorer(t, home, "bucket", "region", "chain",
		mockClientFactory(&mockTransferClient{responses: responses}),
		mockListerFactory(&mockObjectLister{keys: keys}))
}

func requireDiagnosticSnapshotMembership(t *testing.T, home string, expectedHeight uint64) {
	t.Helper()
	dir := filepath.Join(home, "data", "snapshots")
	metadata, err := db.NewGoLevelDB("metadata", dir)
	if err != nil {
		t.Fatalf("reopening restored LevelDB: %v", err)
	}
	defer func() {
		if err := metadata.Close(); err != nil {
			t.Error(err)
		}
	}()
	store, err := snapshots.NewStore(metadata, dir)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := store.List()
	if err != nil {
		t.Fatalf("listing restored SDK metadata: %v", err)
	}
	latest, err := store.GetLatest()
	if err != nil {
		t.Fatalf("selecting latest from restored SDK metadata: %v", err)
	}
	if latest == nil {
		t.Fatal("restored SDK metadata contains no latest snapshot")
	}
	t.Logf("SDK GetLatest selected height %d; expected restored height %d", latest.Height, expectedHeight)
	if latest.Height != expectedHeight {
		t.Errorf("foreign snapshot overrides restored snapshot: GetLatest=%d; want %d", latest.Height, expectedHeight)
	}
	heights := make([]uint64, 0, len(listed))
	for _, snapshot := range listed {
		heights = append(heights, snapshot.Height)
		for index := uint32(0); index < snapshot.Chunks; index++ {
			chunk, err := store.LoadChunk(snapshot.Height, snapshot.Format, index)
			if err != nil {
				t.Errorf("loading advertised height %d chunk %d: %v", snapshot.Height, index, err)
				continue
			}
			if chunk == nil {
				t.Errorf("restored metadata advertises height %d chunk %d that does not exist", snapshot.Height, index)
				continue
			}
			if err := chunk.Close(); err != nil {
				t.Error(err)
			}
		}
	}
	t.Logf("SDK reopened restored metadata with heights=%v; expected only %d", heights, expectedHeight)
	if len(listed) != 1 || listed[0].Height != expectedHeight {
		t.Errorf("foreign metadata survived successful restore: heights=%v; want only %d", heights, expectedHeight)
	}
}

// saveDiagnosticStoreSnapshot lets the SDK produce the metadata and hashes.
// Chunk payloads are opaque to this snapshot store diagnostic; no application
// restore is claimed by these tests.
func saveDiagnosticStoreSnapshot(t *testing.T, store *snapshots.Store, height uint64) {
	t.Helper()
	chunks := make(chan io.ReadCloser, 1)
	chunks <- io.NopCloser(strings.NewReader(fmt.Sprintf("SDK snapshot store chunk at height %d", height)))
	close(chunks)
	completed, err := store.Save(height, snapshottypes.CurrentFormat, chunks)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Height != height || completed.Chunks != 1 {
		t.Fatalf("fixture: SDK completion=%+v", completed)
	}
}

// diagnosticJournalSequence reads a real batch header through LevelDB's journal
// parser. The sequence must exceed the incoming journal's recovery highwater.
func diagnosticJournalSequence(t *testing.T, path string) uint64 {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader := journal.NewReader(file, nil, true, true)
	batch, err := reader.Next()
	if err != nil {
		t.Fatalf("reading actual LevelDB journal batch: %v", err)
	}
	header := make([]byte, 12)
	if _, err := io.ReadFull(batch, header); err != nil {
		t.Fatalf("reading actual LevelDB batch header: %v", err)
	}
	return binary.LittleEndian.Uint64(header[:8])
}
