package tasks

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	protoio "github.com/gogo/protobuf/io"
	"github.com/sei-protocol/sei-chain/sei-cosmos/snapshots"
	snapshottypes "github.com/sei-protocol/sei-chain/sei-cosmos/snapshots/types"
	abci "github.com/sei-protocol/sei-chain/sei-tendermint/abci/types"
	db "github.com/tendermint/tm-db"

	"github.com/sei-protocol/sei-k8s-controller/sidecar/rpc"

	seis3 "github.com/sei-protocol/sei-k8s-controller/sidecar/s3"
)

const producerProcessEnv = "SEI_SNAPSHOT_TEST_PRODUCER"

// Verify the real SDK producer and application restorer without the uploader,
// so a failed publication test cannot be attributed to an unusable fixture.
func TestSnapshotPublication_ProducerRoundTrip(t *testing.T) {
	home := t.TempDir()
	createPublicationSnapshots(t, home, 1000, 2000)
	if err := checkPublicationRestoresApplication(home, 2000); err != nil {
		t.Fatalf("real SDK generation and application restore: %v", err)
	}
}

// Two producer crashes leave abandoned snapshots above the completed history.
// A third generation stays active while publication must select and restore 2000.
func TestSnapshotPublication_ProducerRestartsSkipAbandonedSnapshots(t *testing.T) {
	fixture := newSnapshotPublicationFixture(t, 1000, 2000)
	fixture.killProducerDuringSnapshot(3000)
	fixture.killProducerDuringSnapshot(4000)
	assertCompletedPublicationHeights(t, fixture.home, 2000, 1000)
	fixture.leaveSnapshotInProgress(5000)

	result, err := fixture.uploader.Upload(t.Context())
	if err != nil || result.Outcome != OutcomeUploaded || result.Height != 2000 {
		t.Fatalf("Upload = %+v, %v; want newest completed height 2000", result, err)
	}

	fixture.restoreUploadedApplication(result)
}

// Retention one removes older records and chunks. The remaining completed SDK
// snapshot must still publish and restore without a second directory.
func TestSnapshotPublication_OneRetainedSnapshotPublishes(t *testing.T) {
	fixture := newSnapshotPublicationFixture(t, 1000, 2000)
	store, metadata := openPublicationStore(t, fixture.home)
	pruned, err := snapshots.NewManager(store, &publicationSnapshotter{}).Prune(1)
	if err != nil {
		_ = metadata.Close()
		t.Fatal(err)
	}
	fixture.completed, err = store.List()
	closeErr := metadata.Close()
	if err != nil || closeErr != nil || pruned != 1 || len(fixture.completed) != 1 || fixture.completed[0].Height != 2000 {
		t.Fatalf("pruned=%d completed=%v errors=%v,%v; want only completed 2000", pruned, fixture.completed, err, closeErr)
	}
	if _, err := os.Stat(filepath.Join(fixture.home, "data", "snapshots", "1000", "1", "0")); !os.IsNotExist(err) {
		t.Fatalf("older chunk after SDK pruning: %v; want absent", err)
	}
	fixture.useUploadService(mockUploaderFactory(fixture.uploaded))

	result, err := fixture.uploader.Upload(t.Context())
	if err != nil || result.Outcome != OutcomeUploaded || result.Height != 2000 {
		t.Fatalf("single retained snapshot = %+v, %v; want uploaded 2000", result, err)
	}
	fixture.restoreUploadedApplication(result)
}

// Abandoned directories alone provide no snapshot that may be published.
// Neither the bucket nor the local success record may advance.
func TestSnapshotPublication_NoCompletedSnapshotHasNoPublication(t *testing.T) {
	fixture := newSnapshotPublicationFixture(t)
	fixture.killProducerDuringSnapshot(3000)
	fixture.killProducerDuringSnapshot(4000)
	assertCompletedPublicationHeights(t, fixture.home)

	result, err := fixture.uploader.Upload(t.Context())
	if err != nil || result.Outcome != OutcomeNoop || result.NoopReason != NoopNoCompletedSnapshots {
		t.Fatalf("Upload = %+v, %v; want no completed snapshot", result, err)
	}

	fixture.assertNoPublication()
}

// Fail publication before archive transfer, during its stream, or at latest.txt.
// Each failure must preserve success state, and a retry must restore real data.
func TestSnapshotPublication_FailuresDoNotAdvanceAndCanRetry(t *testing.T) {
	for _, stage := range []string{"archive", "archive-stream", "latest"} {
		t.Run(stage, func(t *testing.T) {
			fixture := newSnapshotPublicationFixture(t, 1000)
			fixture.killProducerDuringSnapshot(2000)
			service := newFailingPublicationService(stage, fixture.uploaded)
			fixture.useUploadService(func(context.Context, string) (seis3.Uploader, error) { return service, nil })

			result, err := fixture.uploader.Upload(t.Context())
			if err == nil || result.Outcome != OutcomeError {
				t.Fatalf("Upload = %+v, %v; want publication failure", result, err)
			}
			if _, ok := fixture.uploaded.uploads["bucket/chain/state-sync/latest.txt"]; ok {
				t.Error("advanced latest.txt after failed publication")
			}
			if stage != "latest" && len(service.calls) != 1 {
				t.Errorf("attempted latest.txt after archive failure: %v", service.calls)
			}
			if fixture.uploader.readUploadState().LastUploadedHeight != 0 {
				t.Error("advanced local success state after failed publication")
			}

			service.failKey = ""
			result, err = fixture.uploader.Upload(t.Context())
			if err != nil || result.Outcome != OutcomeUploaded {
				t.Fatalf("retry = %+v, %v", result, err)
			}
			fixture.restoreUploadedApplication(result)
		})
	}
}

// The producer may prune a completed snapshot after selection but before its
// files are opened. Missing chunks must abort publication without success state.
func TestSnapshotPublication_ChunkDisappearsAfterSelectionDoesNotPublish(t *testing.T) {
	fixture := newSnapshotPublicationFixture(t, 1000, 2000)
	fixture.useUploadService(func(context.Context, string) (seis3.Uploader, error) {
		// The factory runs after selection and before the archiver starts.
		fixture.damageCompletedChunks("missing")
		return fixture.uploaded, nil
	})

	result, err := fixture.uploader.Upload(t.Context())
	if err == nil || result.Outcome != OutcomeError {
		t.Fatalf("Upload = %+v, %v; want failed archive", result, err)
	}

	fixture.assertNoPublication()
}

// Completion metadata does not protect files from later loss or corruption.
// Remove, truncate, or change bytes without changing size, then attempt upload.
func TestSnapshotPublication_InvalidCompletedChunksAreNotPublished(t *testing.T) {
	for _, damage := range []string{"missing", "truncated", "same-size-corruption"} {
		t.Run(damage, func(t *testing.T) {
			fixture := newSnapshotPublicationFixture(t, 1000, 2000)
			fixture.damageCompletedChunks(damage)

			result, err := fixture.uploader.Upload(t.Context())
			if err == nil || result.Outcome != OutcomeError {
				t.Fatalf("Upload = %+v, %v; want rejection of %s chunks", result, err, damage)
			}

			fixture.assertNoPublication()
		})
	}
}

// Every chunk matches its individual hash, but the recorded snapshot hash is
// wrong. Validate the aggregate before the archive can become a final object.
func TestSnapshotPublication_ValidChunkHashesDoNotOverrideAggregateMismatch(t *testing.T) {
	fixture := newSnapshotPublicationFixture(t, 1000)
	fixture.completed[0].Hash[0] ^= 0xff
	fixture.useUploadService(mockUploaderFactory(fixture.uploaded))

	result, err := fixture.uploader.Upload(t.Context())
	if err == nil || !strings.Contains(err.Error(), "producer hash") || result.Outcome != OutcomeError {
		t.Fatalf("Upload=%+v error=%v, want aggregate hash rejection", result, err)
	}

	fixture.assertNoPublication()
}

// A valid application stream spans three chunk files. All must be archived in
// their recorded order so the SDK can restore the original application payload.
func TestSnapshotPublication_AllChunksRestoreInProducerOrder(t *testing.T) {
	fixture := newSnapshotPublicationFixture(t, 1000)
	fixture.splitCompletedSnapshotIntoThreeChunks(1000, 2000)

	result, err := fixture.uploader.Upload(t.Context())
	if err != nil || result.Height != 2000 {
		t.Fatalf("Upload=%+v error=%v, want height 2000", result, err)
	}

	fixture.restoreUploadedApplication(result)
}

// publicationSnapshotter supplies recognizable application data while the real
// SDK manager owns protobuf encoding, compression, chunking and restoration.
type publicationSnapshotter struct {
	payload []byte
}

func (s *publicationSnapshotter) Snapshot(_ uint64, writer protoio.Writer) error {
	return snapshottypes.WriteExtensionItem(writer, s.payload)
}

func (s *publicationSnapshotter) Restore(_ uint64, _ uint32, reader protoio.Reader) (snapshottypes.SnapshotItem, error) {
	item := &snapshottypes.SnapshotItem{}
	if err := reader.ReadMsg(item); err != nil {
		return snapshottypes.SnapshotItem{}, err
	}
	payload := item.GetExtensionPayload()
	if payload == nil {
		return snapshottypes.SnapshotItem{}, errors.New("missing application payload")
	}
	s.payload = append([]byte(nil), payload.Payload...)
	err := reader.ReadMsg(&snapshottypes.SnapshotItem{})
	if !errors.Is(err, io.EOF) {
		return snapshottypes.SnapshotItem{}, fmt.Errorf("expected end of application snapshot, got %v", err)
	}
	return snapshottypes.SnapshotItem{}, nil
}

// openPublicationStore opens the real SDK snapshot store; the caller must close
// the returned database before another producer process opens it.
func openPublicationStore(t *testing.T, home string) (*snapshots.Store, db.DB) {
	t.Helper()
	dir := filepath.Join(home, "data", "snapshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata, err := db.NewGoLevelDB("metadata", dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := snapshots.NewStore(metadata, dir)
	if err != nil {
		_ = metadata.Close()
		t.Fatal(err)
	}
	return store, metadata
}

// createPublicationSnapshots generates application snapshots with the real SDK
// manager and closes the metadata database before returning their records.
func createPublicationSnapshots(t *testing.T, home string, heights ...uint64) []*snapshottypes.Snapshot {
	t.Helper()
	store, metadata := openPublicationStore(t, home)
	defer func() { _ = metadata.Close() }()
	producer := &publicationSnapshotter{payload: []byte("account balance: 42; validator voting power: 7")}
	manager := snapshots.NewManager(store, producer)
	var completed []*snapshottypes.Snapshot
	for _, height := range heights {
		snapshot, err := manager.Create(height)
		if err != nil {
			t.Fatalf("creating completed snapshot %d: %v", height, err)
		}
		completed = append(completed, snapshot)
	}
	return completed
}

// producerReadyChunk signals only after Store.Save has closed its first chunk
// file. The unclosed input channel then keeps generation unfinished.
type producerReadyChunk struct {
	io.Reader
}

func (producerReadyChunk) Close() error {
	_, err := fmt.Fprintln(os.Stdout, "CHUNK_CLOSED")
	return err
}

// TestSnapshotProducerProcess runs in a separate process so SIGKILL leaves the
// same state on disk as a node restart, including LevelDB recovery on reopening.
func TestSnapshotProducerProcess(t *testing.T) {
	home := os.Getenv(producerProcessEnv)
	if home == "" {
		t.Skip("helper process")
	}
	height, err := strconv.ParseUint(os.Getenv("SEI_SNAPSHOT_TEST_HEIGHT"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	store, metadata := openPublicationStore(t, home)
	defer func() { _ = metadata.Close() }()
	chunks := make(chan io.ReadCloser)
	go func() {
		chunks <- producerReadyChunk{Reader: strings.NewReader("first chunk of an unfinished snapshot")}
		// The parent keeps stdin open until it kills this producer. The syscall
		// also keeps the helper alive while Save waits for the next chunk.
		_, _ = io.Copy(io.Discard, os.Stdin)
		// Losing the control pipe must not finalize this generation.
		os.Exit(2)
	}()
	if _, err := store.Save(height, snapshottypes.CurrentFormat, chunks); err != nil {
		t.Fatal(err)
	}
}

// startUnfinishedProducer waits until a separate producer has closed its first
// chunk file. The returned stop function kills it; cleanup also stops it.
func startUnfinishedProducer(t *testing.T, home string, height uint64) func() {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSnapshotProducerProcess$")
	cmd.Env = append(os.Environ(), producerProcessEnv+"="+home, "SEI_SNAPSHOT_TEST_HEIGHT="+strconv.FormatUint(height, 10))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			killErr := cmd.Process.Kill()
			waitErr := cmd.Wait()
			_ = stdin.Close()
			if killErr != nil {
				t.Errorf("killing producer %d: %v", height, killErr)
			}
			if cmd.ProcessState == nil {
				t.Errorf("producer %d has no exit status: %v", height, waitErr)
				return
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Errorf("producer %d did not die from SIGKILL: %v (%v)", height, cmd.ProcessState, waitErr)
			}
		})
	}
	t.Cleanup(stop)
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if scanner.Text() == "CHUNK_CLOSED" {
			t.Logf("producer %d wrote a chunk and is still generating", height)
			return stop
		}
	}
	stop()
	t.Fatalf("producer exited before writing chunk: %s (stdout error: %v)", stderr.String(), scanner.Err())
	return nil
}

// assertCompletedPublicationHeights reads authoritative SDK metadata rather
// than inferring completion from the directories left by producer processes.
func assertCompletedPublicationHeights(t *testing.T, home string, want ...uint64) {
	t.Helper()
	store, metadata := openPublicationStore(t, home)
	defer func() { _ = metadata.Close() }()
	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	var heights []uint64
	for _, snapshot := range listed {
		heights = append(heights, snapshot.Height)
	}
	if fmt.Sprint(heights) != fmt.Sprint(want) {
		t.Fatalf("completed producer heights = %v, want %v", heights, want)
	}
	t.Logf("authoritative completed producer heights: %v", heights)
}

// newPublicationUploader connects an uploader to the supplied SDK completion
// records captured after SDK generation over real HTTP, with a test service
// standing in for S3. It does not run a real node RPC server.
func newPublicationUploader(t *testing.T, completed []*snapshottypes.Snapshot, home, bucket, region, chain string, interval time.Duration, factory seis3.UploaderFactory) (*SnapshotUploader, error) {
	t.Helper()
	uploader, err := newTestSnapshotUploader(home, bucket, region, chain, interval, factory)
	if err != nil {
		return nil, err
	}
	response := abci.ResponseListSnapshots{Snapshots: make([]*abci.Snapshot, 0, len(completed))}
	for _, snapshot := range completed {
		wire, err := snapshot.ToABCI()
		if err != nil {
			t.Fatal(err)
		}
		response.Snapshots = append(response.Snapshots, &wire)
	}
	value, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/abci_query" || r.URL.Query().Get("path") != `"/app/snapshots"` {
			http.Error(w, "unexpected snapshot query", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(struct {
			Response abci.ResponseQuery `json:"response"`
		}{Response: abci.ResponseQuery{Value: value}})
	}))
	t.Cleanup(server.Close)
	uploader.snapshotClient = rpc.NewSnapshotClient(rpc.NewClient(server.URL, server.Client()))
	return uploader, nil
}

// restorePublicationArchive downloads the recorded object through the real
// restorer into a new home and returns that home after successful extraction.
func restorePublicationArchive(t *testing.T, uploaded *mockS3Uploader, key string) string {
	t.Helper()
	home := t.TempDir()
	archive, ok := uploaded.uploads["bucket/"+key]
	if !ok {
		t.Fatalf("missing uploaded archive %s", key)
	}
	restorer := mustNewRestorer(t, home, "bucket", "region", "chain",
		mockClientFactory(&mockTransferClient{responses: map[string][]byte{key: archive}}),
		mockListerFactory(&mockObjectLister{keys: []string{key}}))
	if err := restorer.Restore(t.Context(), 0); err != nil {
		t.Fatalf("restoring uploaded archive: %v", err)
	}
	return home
}

// checkPublicationRestoresApplication verifies all advertised chunk hashes and
// restores the selected application snapshot through the real SDK manager.
func checkPublicationRestoresApplication(home string, height uint64) error {
	dir := filepath.Join(home, "data", "snapshots")
	metadata, err := db.NewGoLevelDB("metadata", dir)
	if err != nil {
		return err
	}
	defer func() { _ = metadata.Close() }()
	store, err := snapshots.NewStore(metadata, dir)
	if err != nil {
		return err
	}
	snapshot, err := store.Get(height, snapshottypes.CurrentFormat)
	if err != nil {
		return err
	}
	if snapshot == nil {
		return fmt.Errorf("advertised height %d has no completed metadata record", height)
	}
	listed, err := store.List()
	if err != nil {
		return err
	}
	// Load selected chunks while checking every advertised record before restore.
	var chunks [][]byte
	for _, advertised := range listed {
		for index := uint32(0); index < advertised.Chunks; index++ {
			path := filepath.Join(dir, strconv.FormatUint(advertised.Height, 10), strconv.FormatUint(uint64(advertised.Format), 10), strconv.FormatUint(uint64(index), 10))
			chunk, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("metadata advertises height %d chunk %d absent from archive: %w", advertised.Height, index, err)
			}
			hash := sha256.Sum256(chunk)
			if len(advertised.Metadata.ChunkHashes) != int(advertised.Chunks) || !bytes.Equal(hash[:], advertised.Metadata.ChunkHashes[index]) {
				return fmt.Errorf("advertised height %d chunk %d does not match producer metadata", advertised.Height, index)
			}
			if advertised.Height == height && advertised.Format == snapshot.Format {
				chunks = append(chunks, chunk)
			}
		}
	}
	restoreStore, err := snapshots.NewStore(db.NewMemDB(), dir)
	if err != nil {
		return err
	}
	application := &publicationSnapshotter{}
	manager := snapshots.NewManager(restoreStore, application)
	if err := manager.Restore(*snapshot); err != nil {
		return err
	}
	var complete bool
	for _, chunk := range chunks {
		complete, err = manager.RestoreChunk(chunk)
		if err != nil {
			return err
		}
	}
	if !complete || string(application.payload) != "account balance: 42; validator voting power: 7" {
		return fmt.Errorf("application restore complete=%v, payload=%q", complete, application.payload)
	}
	return nil
}

// snapshotPublicationFixture owns a real SDK producer, its completion records,
// an uploader querying those records over HTTP, and the objects it publishes.
type snapshotPublicationFixture struct {
	t         *testing.T
	home      string
	completed []*snapshottypes.Snapshot
	uploaded  *mockS3Uploader
	uploader  *SnapshotUploader
}

// newSnapshotPublicationFixture creates the given completed heights and connects
// an uploader to their producer metadata and an initially empty mock bucket.
func newSnapshotPublicationFixture(t *testing.T, heights ...uint64) *snapshotPublicationFixture {
	t.Helper()
	fixture := &snapshotPublicationFixture{t: t, home: t.TempDir(), uploaded: newMockS3Uploader()}
	fixture.completed = createPublicationSnapshots(t, fixture.home, heights...)
	fixture.useUploadService(mockUploaderFactory(fixture.uploaded))
	return fixture
}

// useUploadService replaces the upload service and refreshes the producer RPC
// response from this fixture. It preserves the local upload history and bucket.
func (f *snapshotPublicationFixture) useUploadService(factory seis3.UploaderFactory) {
	f.t.Helper()
	uploader, err := newPublicationUploader(f.t, f.completed, f.home, "bucket", "region", "chain", 0, factory)
	if err != nil {
		f.t.Fatal(err)
	}
	f.uploader = uploader
}

// killProducerDuringSnapshot kills a separate SDK producer after it closes one
// chunk file, leaving its snapshot directory without a completion record.
func (f *snapshotPublicationFixture) killProducerDuringSnapshot(height uint64) {
	f.t.Helper()
	stop := startUnfinishedProducer(f.t, f.home, height)
	stop()
}

// leaveSnapshotInProgress starts an unfinished producer and keeps its metadata
// database open until test cleanup, while the uploader runs independently.
func (f *snapshotPublicationFixture) leaveSnapshotInProgress(height uint64) {
	f.t.Helper()
	startUnfinishedProducer(f.t, f.home, height)
}

// restoreUploadedApplication downloads and extracts the archive, checks every
// advertised chunk, and restores the application through the real SDK manager.
func (f *snapshotPublicationFixture) restoreUploadedApplication(result SnapshotUploadResult) {
	f.t.Helper()
	restored := restorePublicationArchive(f.t, f.uploaded, result.Key)
	if err := checkPublicationRestoresApplication(restored, uint64(result.Height)); err != nil {
		f.t.Fatalf("published snapshot cannot restore the application: %v", err)
	}
}

// assertNoPublication checks both external objects and local success state.
func (f *snapshotPublicationFixture) assertNoPublication() {
	f.t.Helper()
	if len(f.uploaded.uploads) != 0 {
		f.t.Errorf("published %d objects without a valid snapshot", len(f.uploaded.uploads))
	}
	if f.uploader.readUploadState().LastUploadedHeight != 0 {
		f.t.Error("advanced local success state without a valid snapshot")
	}
}

// damageCompletedChunks damages every candidate, so selecting a different height
// cannot accidentally evade the missing file or corruption under test.
func (f *snapshotPublicationFixture) damageCompletedChunks(damage string) {
	f.t.Helper()
	for _, snapshot := range f.completed {
		path := filepath.Join(f.home, "data", "snapshots", strconv.FormatUint(snapshot.Height, 10), strconv.FormatUint(uint64(snapshot.Format), 10), "0")
		original, err := os.ReadFile(path)
		if err != nil || len(original) < 2 {
			f.t.Fatalf("invalid producer fixture: chunk length=%d error=%v", len(original), err)
		}
		switch damage {
		case "missing":
			err = os.Remove(path)
		case "truncated":
			err = os.WriteFile(path, original[:len(original)/2], 0o644)
		case "same-size-corruption":
			original[len(original)/2] ^= 0xff
			err = os.WriteFile(path, original, 0o644)
		default:
			f.t.Fatalf("unknown chunk damage %q", damage)
		}
		if err != nil {
			f.t.Fatal(err)
		}
	}
}

// splitCompletedSnapshotIntoThreeChunks saves the same real SDK stream at a new
// height with three chunks and refreshes the completion records seen over RPC.
func (f *snapshotPublicationFixture) splitCompletedSnapshotIntoThreeChunks(from, to uint64) {
	f.t.Helper()
	chunk, err := os.ReadFile(filepath.Join(f.home, "data", "snapshots", strconv.FormatUint(from, 10), "1", "0"))
	if err != nil || len(chunk) < 20 {
		f.t.Fatalf("invalid complete producer fixture: bytes=%d error=%v", len(chunk), err)
	}
	chunks := make(chan io.ReadCloser, 3)
	for _, body := range [][]byte{chunk[:5], chunk[5:15], chunk[15:]} {
		chunks <- io.NopCloser(bytes.NewReader(body))
	}
	close(chunks)
	store, metadata := openPublicationStore(f.t, f.home)
	snapshot, err := store.Save(to, snapshottypes.CurrentFormat, chunks)
	_ = metadata.Close()
	if err != nil {
		f.t.Fatal(err)
	}
	f.completed = append(f.completed, snapshot)
	f.useUploadService(mockUploaderFactory(f.uploaded))
}

// newFailingPublicationService fails one publication stage until failKey is
// cleared; the stream failure consumes bytes before returning its error.
func newFailingPublicationService(stage string, uploaded *mockS3Uploader) *publicationFailureUploader {
	service := &publicationFailureUploader{uploaded: uploaded, failKey: "archive", failAfterRead: stage == "archive-stream"}
	if stage == "latest" {
		service.failKey = "chain/state-sync/latest.txt"
	}
	return service
}

// publicationFailureUploader records only successfully consumed objects, as S3
// would when a multipart stream fails before completion.
type publicationFailureUploader struct {
	uploaded      *mockS3Uploader
	failKey       string
	failAfterRead bool
	calls         []string
}

func (m *publicationFailureUploader) UploadObject(ctx context.Context, input *transfermanager.UploadObjectInput, options ...func(*transfermanager.Options)) (*transfermanager.UploadObjectOutput, error) {
	key := *input.Key
	m.calls = append(m.calls, key)
	if key == m.failKey || (m.failKey == "archive" && strings.HasSuffix(key, ".tar.gz")) {
		if m.failAfterRead {
			prefix := make([]byte, 32)
			if _, err := io.ReadFull(input.Body, prefix); err != nil {
				return nil, fmt.Errorf("reading archive prefix before injected failure: %w", err)
			}
		}
		return nil, errors.New("injected publication failure")
	}
	body, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, err
	}
	copyInput := *input
	copyInput.Body = bytes.NewReader(body)
	return m.uploaded.UploadObject(ctx, &copyInput, options...)
}
