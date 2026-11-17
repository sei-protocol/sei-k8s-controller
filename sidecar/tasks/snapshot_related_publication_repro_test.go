package tasks

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	seis3 "github.com/sei-protocol/sei-k8s-controller/sidecar/s3"
)

// Each publisher has its own local checkpoint and shares the S3 destination.
func TestSnapshotRelatedPublication_IndependentPublishers(t *testing.T) {
	for _, test := range []struct {
		name        string
		next        int64
		wantMarker  string
		wantOutcome seis3.MarkerOutcome
		wantPuts    int
	}{
		{"older", 1000, "2000", seis3.MarkerNewer, 1},
		{"equal", 2000, "2000", seis3.MarkerEqual, 1},
		{"newer", 3000, "3000", seis3.MarkerAdvanced, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			bucket := newPublicationReproBucket(t)
			first := newPublicationReproNode(t, bucket, 2000)
			next := newPublicationReproNode(t, bucket, test.next)

			requirePublicationReproUpload(t, first, 2000)
			result := requirePublicationReproUpload(t, next, test.next)

			if result.MarkerOutcome != test.wantOutcome || bucket.markerPutCount() != test.wantPuts {
				t.Errorf("marker outcome %s after %d writes; want %s after %d", result.MarkerOutcome, bucket.markerPutCount(), test.wantOutcome, test.wantPuts)
			}
			requirePublicationReproLatest(t, bucket, test.wantMarker)
			for _, height := range []int64{2000, test.next} {
				if len(bucket.object(fmt.Sprintf("%d.tar.gz", height))) == 0 {
					t.Errorf("archive at height %d is missing", height)
				}
			}
		})
	}
}

func TestSnapshotRelatedPublication_ConcurrentOlderWriteLosesCondition(t *testing.T) {
	for _, test := range []struct {
		name    string
		initial int64
		older   int64
		newer   int64
	}{
		{name: "create absent marker", older: 1000, newer: 2000},
		{name: "replace existing marker", initial: 1000, older: 2000, newer: 3000},
	} {
		t.Run(test.name, func(t *testing.T) {
			bucket := newPublicationReproBucket(t)
			if test.initial > 0 {
				requirePublicationReproUpload(t, newPublicationReproNode(t, bucket, test.initial), test.initial)
			}
			older := newPublicationReproNode(t, bucket, test.older)
			newer := newPublicationReproNode(t, bucket, test.newer)
			putsBefore := bucket.markerPutCount()
			oldWriteReached := make(chan struct{})
			releaseOldWrite := make(chan struct{})
			var release sync.Once
			bucket.mu.Lock()
			bucket.beforeMarkerPut = func(body []byte) {
				if string(body) == strconv.FormatInt(test.older, 10) {
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
			requirePublicationReproUpload(t, newer, test.newer)
			release.Do(func() { close(releaseOldWrite) })
			completed := <-done
			state := older.readUploadState()
			if completed.err != nil || completed.result.Outcome != OutcomeUploaded || completed.result.Height != test.older || completed.result.MarkerOutcome != seis3.MarkerNewer || state.LastUploadedHeight != test.older {
				t.Fatalf("older publication = %+v, %v, checkpoint=%+v; want committed older archive with newer marker retained", completed.result, completed.err, state)
			}
			requirePublicationReproLatest(t, bucket, strconv.FormatInt(test.newer, 10))
			if puts := bucket.markerPutCount() - putsBefore; puts != 2 {
				t.Fatalf("marker PUTs = %d; want newer success and older conditional conflict", puts)
			}
		})
	}
}

func TestSnapshotRelatedPublication_LostReplyThenNewerPublisher(t *testing.T) {
	bucket := newPublicationReproBucket(t)
	older := newPublicationReproNode(t, bucket, 2000)
	newer := newPublicationReproNode(t, bucket, 3000)
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
}

// publicationReproBucket implements atomic S3 conditional replacement at a local
// HTTP endpoint. Archive uploads still use the real AWS transfer manager.
type publicationReproBucket struct {
	server          *httptest.Server
	mu              sync.Mutex
	objects         map[string][]byte
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

func newPublicationReproNode(t *testing.T, bucket *publicationReproBucket, height int64) *SnapshotUploader {
	t.Helper()
	home := t.TempDir()
	// This stack supplies the producer's completed record, rather than directory order.
	createPublicationSnapshots(t, home, uint64(height))
	node, err := newFixtureSnapshotUploader(t, home, "bucket", "eu-central-1", "chain", 0, bucket.uploaderFactory)
	if err != nil {
		t.Fatal(err)
	}
	node.latestMarkerFactory = bucket.markerFactory
	return node
}

func requirePublicationReproUpload(t *testing.T, node *SnapshotUploader, height int64) SnapshotUploadResult {
	t.Helper()
	result, err := node.Upload(t.Context())
	if err != nil || result.Outcome != OutcomeUploaded || result.Height != height {
		t.Fatalf("upload = %+v, %v; want completed height %d", result, err, height)
	}
	if state := node.readUploadState(); state.LastUploadedHeight != height {
		t.Fatalf("local checkpoint = %+v; want height %d", state, height)
	}
	return result
}

func requirePublicationReproLatest(t *testing.T, bucket *publicationReproBucket, height string) {
	t.Helper()
	if got := string(bucket.object("latest.txt")); got != height {
		t.Fatalf("remote latest = %s, want %s", got, height)
	}
}
