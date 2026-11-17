package tasks

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	protoio "github.com/gogo/protobuf/io"
	"github.com/sei-protocol/sei-chain/sei-cosmos/snapshots"
	snapshottypes "github.com/sei-protocol/sei-chain/sei-cosmos/snapshots/types"
	db "github.com/tendermint/tm-db"

	seis3 "github.com/sei-protocol/sei-k8s-controller/sidecar/s3"
)

// Two nodes publish independently into one destination. Both selected snapshots
// are complete; an older node must not replace the newer published pointer.
func TestSnapshotRelatedPublication_IndependentOlderPublisherDoesNotRegressLatest(t *testing.T) {
	bucket := newPublicationReproBucket(t)
	newer := newCompletedPublicationReproNode(t, bucket, 2000)
	older := newCompletedPublicationReproNode(t, bucket, 1000)

	requirePublicationReproUpload(t, newer, 2000)
	requirePublicationReproLatest(t, bucket, "2000")
	if state := older.readUploadState(); state.LastUploadedHeight != 0 {
		t.Fatalf("older node must have an independent fresh checkpoint, got %+v", state)
	}

	result, err := older.Upload(t.Context())
	if err != nil {
		t.Fatalf("older independent upload failed before its pointer write: %v", err)
	}
	t.Logf("older node returned %+v; remote latest=%s; local newer=%+v older=%+v",
		result, bucket.object("latest.txt"), newer.readUploadState(), older.readUploadState())
	requirePublicationReproArchive(t, bucket, 1000)
	requirePublicationReproArchive(t, bucket, 2000)
	if result.Outcome != OutcomeUploaded || result.MarkerOutcome != seis3.MarkerNewer || older.readUploadState().LastUploadedHeight != 1000 {
		t.Errorf("older publication = %+v, checkpoint %+v; want archived 1000 with newer marker retained", result, older.readUploadState())
	}
	if got := string(bucket.object("latest.txt")); got != "2000" {
		t.Errorf("independent older publisher regressed latest.txt to %s; want 2000", got)
	}
}

// Independent nodes still need to advance publication when the second node has
// a newer complete snapshot. This controls for a fixture that rejects all writes.
func TestSnapshotRelatedPublication_IndependentNewerPublisherAdvancesLatest(t *testing.T) {
	bucket := newPublicationReproBucket(t)
	older := newCompletedPublicationReproNode(t, bucket, 1000)
	newer := newCompletedPublicationReproNode(t, bucket, 2000)

	requirePublicationReproUpload(t, older, 1000)
	requirePublicationReproLatest(t, bucket, "1000")
	requirePublicationReproUpload(t, newer, 2000)

	requirePublicationReproLatest(t, bucket, "2000")
	requirePublicationReproArchive(t, bucket, 1000)
	requirePublicationReproArchive(t, bucket, 2000)
}

// A repeated call on one home is skipped. The case with independent nodes
// must therefore distinguish separate local histories from ordinary retries.
func TestSnapshotRelatedPublication_SameNodeSkipsItsCommittedSnapshot(t *testing.T) {
	bucket := newPublicationReproBucket(t)
	node := newCompletedPublicationReproNode(t, bucket, 2000)
	requirePublicationReproUpload(t, node, 2000)
	callsBeforeRetry := bucket.requestCount()

	result, err := node.Upload(t.Context())
	if err != nil || result.Outcome != OutcomeNoop || result.NoopReason != NoopAlreadyUploaded {
		t.Fatalf("same-node retry = %+v, %v; want already-uploaded noop", result, err)
	}
	if calls := bucket.requestCount(); calls != callsBeforeRetry {
		t.Errorf("same-node retry made %d extra S3 requests", calls-callsBeforeRetry)
	}
	requirePublicationReproLatest(t, bucket, "2000")
}

func TestSnapshotRelatedPublication_ConcurrentOlderWriteLosesCondition(t *testing.T) {
	for _, test := range []struct {
		name    string
		initial uint64
		older   uint64
		newer   uint64
	}{
		{name: "create absent marker", older: 1000, newer: 2000},
		{name: "replace existing marker", initial: 1000, older: 2000, newer: 3000},
	} {
		t.Run(test.name, func(t *testing.T) {
			bucket := newPublicationReproBucket(t)
			if test.initial > 0 {
				requirePublicationReproUpload(t, newCompletedPublicationReproNode(t, bucket, test.initial), int64(test.initial))
			}
			older := newCompletedPublicationReproNode(t, bucket, test.older)
			newer := newCompletedPublicationReproNode(t, bucket, test.newer)
			putsBefore := bucket.markerPutCount()
			oldWriteReached := make(chan struct{})
			releaseOldWrite := make(chan struct{})
			var release sync.Once
			bucket.mu.Lock()
			bucket.beforeMarkerPut = func(body []byte) {
				if string(body) == strconv.FormatUint(test.older, 10) {
					close(oldWriteReached)
					<-releaseOldWrite
				}
			}
			bucket.mu.Unlock()
			type completion struct {
				result SnapshotUploadResult
				err    error
			}
			done := make(chan completion, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				result, err := older.Upload(t.Context())
				done <- completion{result, err}
			}()
			t.Cleanup(func() {
				release.Do(func() { close(releaseOldWrite) })
				<-finished
			})
			// Pause after the old observation, before atomic condition evaluation.
			// The newer publisher wins while the old request is suspended here.
			select {
			case <-oldWriteReached:
			case early := <-done:
				t.Fatalf("older upload ended before the conditional PUT: %+v, %v", early.result, early.err)
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			requirePublicationReproUpload(t, newer, int64(test.newer))
			release.Do(func() { close(releaseOldWrite) })
			completed := <-done
			state := older.readUploadState()
			if completed.err != nil || completed.result.Outcome != OutcomeUploaded || completed.result.Height != int64(test.older) || completed.result.MarkerOutcome != seis3.MarkerNewer || state.LastUploadedHeight != int64(test.older) {
				t.Fatalf("older publication = %+v, %v, checkpoint=%+v; want committed older archive with newer marker retained", completed.result, completed.err, state)
			}
			requirePublicationReproLatest(t, bucket, strconv.FormatUint(test.newer, 10))
			if puts := bucket.markerPutCount() - putsBefore; puts != 2 {
				t.Fatalf("marker PUTs = %d; want newer success and older conditional conflict", puts)
			}
			requirePublicationReproArchive(t, bucket, test.older)
			requirePublicationReproArchive(t, bucket, test.newer)
		})
	}
}

func TestSnapshotRelatedPublication_EqualIndependentPublisherKeepsMarker(t *testing.T) {
	bucket := newPublicationReproBucket(t)
	first := newCompletedPublicationReproNode(t, bucket, 2000)
	second := newCompletedPublicationReproNode(t, bucket, 2000)
	requirePublicationReproUpload(t, first, 2000)
	result, err := second.Upload(t.Context())
	if err != nil || result.Outcome != OutcomeUploaded || result.MarkerOutcome != seis3.MarkerEqual {
		t.Fatalf("equal publication = %+v, %v; want archived with equal marker", result, err)
	}
	if bucket.markerPutCount() != 1 || second.readUploadState().LastUploadedHeight != 2000 {
		t.Fatalf("marker PUTs=%d checkpoint=%+v; equal must commit without rewriting marker", bucket.markerPutCount(), second.readUploadState())
	}
	requirePublicationReproArchive(t, bucket, 2000)
}

func TestSnapshotRelatedPublication_LostReplyThenNewerPublisher(t *testing.T) {
	bucket := newPublicationReproBucket(t)
	older := newCompletedPublicationReproNode(t, bucket, 2000)
	newer := newCompletedPublicationReproNode(t, bucket, 3000)
	bucket.dropMarkerReply = true
	result, err := older.Upload(t.Context())
	if err == nil || result.Outcome != OutcomeError || result.MarkerOutcome != "" {
		t.Fatalf("lost response = %+v, %v; want unresolved error", result, err)
	}
	requirePublicationReproLatest(t, bucket, "2000")
	if older.readUploadState().LastUploadedHeight != 0 {
		t.Fatal("uncertain write committed local success")
	}
	bucket.mu.Lock()
	bucket.dropMarkerReply = false
	bucket.mu.Unlock()
	requirePublicationReproUpload(t, newer, 3000)
	result, err = older.Upload(t.Context())
	if err != nil || result.Outcome != OutcomeUploaded || result.Height != 2000 || result.MarkerOutcome != seis3.MarkerNewer || older.readUploadState().LastUploadedHeight != 2000 {
		t.Fatalf("retry = %+v, %v; want newer marker retained", result, err)
	}
	requirePublicationReproLatest(t, bucket, "3000")
	requirePublicationReproArchive(t, bucket, 2000)
	requirePublicationReproArchive(t, bucket, 3000)
}

// publicationReproBucket implements atomic S3 conditional replacement at a local
// HTTP endpoint. Archive uploads still use the real AWS transfer manager.
type publicationReproBucket struct {
	server          *httptest.Server
	mu              sync.Mutex
	objects         map[string][]byte
	calls           int
	markerPuts      int
	beforeMarkerPut func([]byte)
	dropMarkerReply bool
}

func newPublicationReproBucket(t *testing.T) *publicationReproBucket {
	t.Helper()
	bucket := &publicationReproBucket{objects: make(map[string][]byte)}
	bucket.server = httptest.NewServer(http.HandlerFunc(bucket.serveHTTP))
	t.Cleanup(bucket.server.Close)
	return bucket
}

func publicationReproETag(body []byte) string {
	checksum := md5.Sum(body)
	return strconv.Quote(hex.EncodeToString(checksum[:]))
}

func (b *publicationReproBucket) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	b.calls++
	hook := b.beforeMarkerPut
	b.mu.Unlock()
	isMarker := strings.HasSuffix(r.URL.Path, "/latest.txt")
	if r.Method == http.MethodPut && isMarker && hook != nil {
		hook(body)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	current, exists := b.objects[r.URL.Path]
	switch r.Method {
	case http.MethodGet:
		if !exists {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		w.Header().Set("ETag", publicationReproETag(current))
		_, _ = w.Write(current)
	case http.MethodPut:
		if isMarker {
			b.markerPuts++
			if r.Header.Get("If-Match") == "" && r.Header.Get("If-None-Match") != "*" {
				http.Error(w, "marker requires a condition", http.StatusBadRequest)
				return
			}
		}
		if (r.Header.Get("If-None-Match") == "*" && exists) ||
			(r.Header.Get("If-Match") != "" && (!exists || r.Header.Get("If-Match") != publicationReproETag(current))) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(w, "<Error><Code>PreconditionFailed</Code></Error>")
			return
		}
		b.objects[r.URL.Path] = append([]byte(nil), body...)
		if isMarker && b.dropMarkerReply {
			// The mutation is visible, but no response reaches the SDK.
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		w.Header().Set("ETag", publicationReproETag(body))
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "unsupported request", http.StatusMethodNotAllowed)
	}
}

func (b *publicationReproBucket) client() *awss3.Client {
	return awss3.NewFromConfig(aws.Config{
		Region:                     "eu-central-1",
		Credentials:                aws.AnonymousCredentials{},
		HTTPClient:                 b.server.Client(),
		RetryMaxAttempts:           1,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
	}, func(options *awss3.Options) {
		options.BaseEndpoint = aws.String(b.server.URL)
		options.UsePathStyle = true
	})
}

func (b *publicationReproBucket) uploaderFactory(context.Context, string) (seis3.Uploader, error) {
	return transfermanager.New(b.client()), nil
}

func (b *publicationReproBucket) markerFactory(context.Context, string) (seis3.LatestMarker, error) {
	return seis3.NewLatestMarker(b.client()), nil
}

func (b *publicationReproBucket) object(name string) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.objects["/bucket/chain/state-sync/"+name]...)
}

func (b *publicationReproBucket) markerPutCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.markerPuts
}

func (b *publicationReproBucket) requestCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// newCompletedPublicationReproNode gives each uploader its own filesystem home
// and an application snapshot generated by the SDK with closed metadata. One newer
// directory models the next generation, satisfying the baseline selection rule.
func newCompletedPublicationReproNode(t *testing.T, bucket *publicationReproBucket, height uint64) *SnapshotUploader {
	t.Helper()
	home := t.TempDir()
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
	producer := &publicationReproApplication{payload: []byte("account balance: 42")}
	completed, createErr := snapshots.NewManager(store, producer).Create(height)
	closeErr := metadata.Close()
	if createErr != nil || closeErr != nil {
		t.Fatalf("create complete SDK snapshot: %v; close metadata: %v", createErr, closeErr)
	}
	if completed.Height != height || completed.Chunks == 0 {
		t.Fatalf("SDK did not complete the expected snapshot: %+v", completed)
	}
	if err := os.MkdirAll(filepath.Join(dir, strconv.FormatUint(height+1000, 10)), 0o755); err != nil {
		t.Fatal(err)
	}
	node, err := NewSnapshotUploader(home, "bucket", "eu-central-1", "chain", 0, bucket.uploaderFactory, bucket.markerFactory)
	if err != nil {
		t.Fatal(err)
	}
	return node
}

func requirePublicationReproUpload(t *testing.T, node *SnapshotUploader, height int64) {
	t.Helper()
	result, err := node.Upload(t.Context())
	if err != nil || result.Outcome != OutcomeUploaded || result.Height != height {
		t.Fatalf("upload = %+v, %v; want completed height %d", result, err, height)
	}
	if state := node.readUploadState(); state.LastUploadedHeight != height {
		t.Fatalf("local checkpoint = %+v; want height %d", state, height)
	}
}

func requirePublicationReproLatest(t *testing.T, bucket *publicationReproBucket, height string) {
	t.Helper()
	if got := string(bucket.object("latest.txt")); got != height {
		t.Fatalf("remote latest = %s, want %s", got, height)
	}
}

// requirePublicationReproArchive restores the selected application from the
// actual uploaded bytes, keeping invalid snapshot selection out of this proof.
func requirePublicationReproArchive(t *testing.T, bucket *publicationReproBucket, height uint64) {
	t.Helper()
	body := bucket.object(fmt.Sprintf("%d.tar.gz", height))
	if len(body) == 0 {
		t.Fatalf("snapshot %d was not uploaded", height)
	}
	dir := t.TempDir()
	if err := extractTarStream(t.Context(), bytes.NewReader(body), dir); err != nil {
		t.Fatalf("extract SDK upload: %v", err)
	}
	metadata, err := db.NewGoLevelDB("metadata", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = metadata.Close() }()
	store, err := snapshots.NewStore(metadata, dir)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := store.Get(height, snapshottypes.CurrentFormat)
	if err != nil || completed == nil {
		t.Fatalf("uploaded snapshot metadata = %+v, %v", completed, err)
	}
	listed, err := store.List()
	if err != nil || len(listed) != 1 || listed[0].Height != height {
		t.Fatalf("archive must advertise exactly height %d, got %+v, %v", height, listed, err)
	}
	var chunks [][]byte
	for index := uint32(0); index < completed.Chunks; index++ {
		chunkFile, err := store.LoadChunk(height, completed.Format, index)
		if err != nil || chunkFile == nil {
			t.Fatalf("uploaded chunk %d is unavailable: %v", index, err)
		}
		chunk, readErr := io.ReadAll(chunkFile)
		closeErr := chunkFile.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read uploaded chunk %d: %v; close: %v", index, readErr, closeErr)
		}
		chunks = append(chunks, chunk)
	}
	application := &publicationReproApplication{}
	manager := snapshots.NewManager(store, application)
	if err := manager.Restore(*completed); err != nil {
		t.Fatalf("offer uploaded snapshot: %v", err)
	}
	var restored bool
	for index, chunk := range chunks {
		restored, err = manager.RestoreChunk(chunk)
		if err != nil {
			t.Fatalf("restore uploaded chunk %d: %v", index, err)
		}
	}
	if !restored || string(application.payload) != "account balance: 42" {
		t.Fatalf("uploaded application restore complete=%v payload=%q", restored, application.payload)
	}
}

// publicationReproApplication supplies a recognizable application stream while
// the SDK owns encoding, compression, chunking, hashes and restore validation.
type publicationReproApplication struct {
	payload []byte
}

func (a *publicationReproApplication) Snapshot(_ uint64, writer protoio.Writer) error {
	return snapshottypes.WriteExtensionItem(writer, a.payload)
}

func (a *publicationReproApplication) Restore(_ uint64, _ uint32, reader protoio.Reader) (snapshottypes.SnapshotItem, error) {
	item := &snapshottypes.SnapshotItem{}
	if err := reader.ReadMsg(item); err != nil {
		return snapshottypes.SnapshotItem{}, err
	}
	payload := item.GetExtensionPayload()
	if payload == nil {
		return snapshottypes.SnapshotItem{}, errors.New("missing application payload")
	}
	a.payload = append([]byte(nil), payload.Payload...)
	if err := reader.ReadMsg(&snapshottypes.SnapshotItem{}); !errors.Is(err, io.EOF) {
		return snapshottypes.SnapshotItem{}, fmt.Errorf("expected end of application snapshot, got %v", err)
	}
	return snapshottypes.SnapshotItem{}, nil
}
