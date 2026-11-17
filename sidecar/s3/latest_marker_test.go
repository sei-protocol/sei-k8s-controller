package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func TestLatestMarker_AdvancesOnlyAnOlderOrAbsentMarker(t *testing.T) {
	for _, test := range []struct {
		name        string
		current     string
		readError   error
		wantOutcome MarkerOutcome
		wantMatch   string
		wantNone    string
	}{
		{name: "absent", readError: markerAPIError("NoSuchKey"), wantOutcome: MarkerAdvanced, wantNone: "*"},
		{name: "older", current: "1000\n", wantOutcome: MarkerAdvanced, wantMatch: `"version-one"`},
		{name: "equal", current: "2000", wantOutcome: MarkerEqual},
		{name: "newer", current: "3000", wantOutcome: MarkerNewer},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &markerTestClient{t: t}
			client.get = func() (*awss3.GetObjectOutput, error) {
				return markerRead(test.current, `"version-one"`), test.readError
			}
			client.put = func(input *awss3.PutObjectInput) error {
				if got := aws.ToString(input.IfMatch); got != test.wantMatch {
					t.Fatalf("If-Match=%q; want %q", got, test.wantMatch)
				}
				if got := aws.ToString(input.IfNoneMatch); got != test.wantNone {
					t.Fatalf("If-None-Match=%q; want %q", got, test.wantNone)
				}
				body, err := io.ReadAll(input.Body)
				if err != nil || string(body) != "2000" {
					t.Fatalf("marker PUT body=%q error=%v; want 2000", body, err)
				}
				return nil
			}

			outcome, err := NewLatestMarker(client).Advance(t.Context(), "bucket", "chain/state-sync/latest.txt", 2000)
			if err != nil || outcome != test.wantOutcome {
				t.Fatalf("Advance=%q error=%v; want %q", outcome, err, test.wantOutcome)
			}
			wantPuts := 0
			if test.wantOutcome == MarkerAdvanced {
				wantPuts = 1
			}
			if client.gets != 1 || client.puts != wantPuts {
				t.Fatalf("GETs=%d PUTs=%d; want one GET and %d PUTs", client.gets, client.puts, wantPuts)
			}
		})
	}
}

func TestLatestMarker_ConflictRereadsBeforeDeciding(t *testing.T) {
	for _, code := range []string{"PreconditionFailed", "ConditionalRequestConflict"} {
		for _, test := range []struct {
			name        string
			winner      string
			wantOutcome MarkerOutcome
			wantPuts    int
		}{
			{name: "winner is newer", winner: "3000", wantOutcome: MarkerNewer, wantPuts: 1},
			{name: "winner is equal", winner: "2000", wantOutcome: MarkerEqual, wantPuts: 1},
			{name: "winner is still older", winner: "1500", wantOutcome: MarkerAdvanced, wantPuts: 2},
		} {
			t.Run(code+"/"+test.name, func(t *testing.T) {
				client := &markerTestClient{t: t}
				client.get = func() (*awss3.GetObjectOutput, error) {
					if client.gets == 1 {
						return markerRead("1000", `"before"`), nil
					}
					return markerRead(test.winner, `"winner"`), nil
				}
				client.put = func(input *awss3.PutObjectInput) error {
					if input.IfNoneMatch != nil {
						t.Fatal("existing marker must not use If-None-Match")
					}
					if client.puts == 1 {
						if aws.ToString(input.IfMatch) != `"before"` {
							t.Fatalf("first PUT lost the observed ETag: %v", input.IfMatch)
						}
						return fmt.Errorf("S3 rejected stale condition: %w", markerAPIError(code))
					}
					if client.gets != 2 || aws.ToString(input.IfMatch) != `"winner"` {
						t.Fatalf("retry reused stale observation: GETs=%d If-Match=%q", client.gets, aws.ToString(input.IfMatch))
					}
					return nil
				}

				outcome, err := NewLatestMarker(client).Advance(t.Context(), "bucket", "chain/state-sync/latest.txt", 2000)
				if err != nil || outcome != test.wantOutcome || client.gets != 2 || client.puts != test.wantPuts {
					t.Fatalf("Advance=%q error=%v GETs=%d PUTs=%d; want %q, two GETs and %d PUTs", outcome, err, client.gets, client.puts, test.wantOutcome, test.wantPuts)
				}
			})
		}
	}
}

func TestLatestMarker_InvalidReadNeverWrites(t *testing.T) {
	readFailure := errors.New("body read failed")
	closeFailure := errors.New("body close failed")
	for _, test := range []struct {
		name      string
		output    *awss3.GetObjectOutput
		readError error
		wantError error
	}{
		{name: "access denied", readError: markerAPIError("AccessDenied")},
		{name: "bucket missing", readError: markerAPIError("NoSuchBucket")},
		{name: "transport failure", readError: io.ErrUnexpectedEOF, wantError: io.ErrUnexpectedEOF},
		{name: "nil output"},
		{name: "missing body", output: &awss3.GetObjectOutput{}},
		{name: "empty height", output: markerRead("", `"etag"`)},
		{name: "malformed height", output: markerRead("not-a-height", `"etag"`)},
		{name: "zero height", output: markerRead("0", `"etag"`)},
		{name: "negative height", output: markerRead("-1", `"etag"`)},
		{name: "overflow", output: markerRead("9223372036854775808", `"etag"`)},
		// This parses as 1000 after trimming, so the size limit alone rejects it.
		{name: "oversized body", output: markerRead("1000"+strings.Repeat(" ", 17), `"etag"`)},
		{name: "missing ETag for older marker", output: markerRead("1000", "")},
		{name: "read body failure", output: &awss3.GetObjectOutput{Body: &markerFailingBody{readError: readFailure}}, wantError: readFailure},
		{name: "close body failure", output: &awss3.GetObjectOutput{Body: &markerFailingBody{Reader: strings.NewReader("1000"), closeError: closeFailure}}, wantError: closeFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &markerTestClient{t: t, get: func() (*awss3.GetObjectOutput, error) { return test.output, test.readError }}
			outcome, err := NewLatestMarker(client).Advance(t.Context(), "bucket", "chain/state-sync/latest.txt", 2000)
			if err == nil || outcome != "" || client.gets != 1 || client.puts != 0 {
				t.Fatalf("Advance=%q error=%v GETs=%d PUTs=%d; invalid read must fail without writing", outcome, err, client.gets, client.puts)
			}
			if test.wantError != nil && !errors.Is(err, test.wantError) {
				t.Fatalf("error=%v; want wrapped %v", err, test.wantError)
			}
			if test.output != nil {
				if body, ok := test.output.Body.(*markerFailingBody); ok && !body.closed {
					t.Fatal("read failure did not close the marker body")
				}
			}
		})
	}
}

// A PUT error cannot say whether S3 accepted the object. A later call must
// reconcile the actual marker, and must not report the uncertain call advanced it.
func TestLatestMarker_UncertainWriteReconcilesOnRetry(t *testing.T) {
	for _, test := range []struct {
		name        string
		accepted    bool
		laterNewer  bool
		wantOutcome MarkerOutcome
		wantPuts    int
	}{
		{name: "rejected write retries", wantOutcome: MarkerAdvanced, wantPuts: 2},
		{name: "accepted response lost", accepted: true, wantOutcome: MarkerEqual, wantPuts: 1},
		{name: "accepted then superseded", accepted: true, laterNewer: true, wantOutcome: MarkerNewer, wantPuts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := "1000"
			writeFailure := errors.New("response unavailable")
			client := &markerTestClient{t: t}
			client.get = func() (*awss3.GetObjectOutput, error) { return markerRead(current, `"current"`), nil }
			client.put = func(input *awss3.PutObjectInput) error {
				if aws.ToString(input.IfMatch) != `"current"` || input.IfNoneMatch != nil {
					t.Fatal("uncertain write or retry omitted the condition")
				}
				if client.puts == 1 {
					if test.accepted {
						current = "2000"
					}
					return writeFailure
				}
				current = "2000"
				return nil
			}
			marker := NewLatestMarker(client)

			outcome, err := marker.Advance(t.Context(), "bucket", "chain/state-sync/latest.txt", 2000)
			if !errors.Is(err, writeFailure) || outcome != "" || client.gets != 1 || client.puts != 1 {
				t.Fatalf("uncertain attempt=%q error=%v GETs=%d PUTs=%d; must remain unresolved", outcome, err, client.gets, client.puts)
			}
			if test.laterNewer {
				current = "3000"
			}
			outcome, err = marker.Advance(t.Context(), "bucket", "chain/state-sync/latest.txt", 2000)
			if err != nil || outcome != test.wantOutcome || client.gets != 2 || client.puts != test.wantPuts {
				t.Fatalf("reconciled attempt=%q error=%v GETs=%d PUTs=%d; want %q and %d PUTs", outcome, err, client.gets, client.puts, test.wantOutcome, test.wantPuts)
			}
			wantCurrent := "2000"
			if test.laterNewer {
				wantCurrent = "3000"
			}
			if current != wantCurrent {
				t.Fatalf("remote marker=%s; want %s", current, wantCurrent)
			}
		})
	}
}

func TestLatestMarker_CancellationStopsConflictRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &markerTestClient{t: t}
	client.get = func() (*awss3.GetObjectOutput, error) { return markerRead("1000", `"old"`), nil }
	client.put = func(*awss3.PutObjectInput) error {
		cancel()
		return markerAPIError("PreconditionFailed")
	}
	outcome, err := NewLatestMarker(client).Advance(ctx, "bucket", "chain/state-sync/latest.txt", 2000)
	if !errors.Is(err, context.Canceled) || outcome != "" || client.gets != 1 || client.puts != 1 {
		t.Fatalf("Advance=%q error=%v GETs=%d PUTs=%d; canceled conflict must stop", outcome, err, client.gets, client.puts)
	}
}

func TestLatestMarker_InvalidCandidateDoesNotAccessS3(t *testing.T) {
	for _, height := range []int64{0, -1} {
		client := &markerTestClient{t: t}
		outcome, err := NewLatestMarker(client).Advance(t.Context(), "bucket", "chain/state-sync/latest.txt", height)
		if err == nil || outcome != "" || client.gets != 0 || client.puts != 0 {
			t.Fatalf("height %d: Advance=%q error=%v GETs=%d PUTs=%d; invalid candidate must not access S3", height, outcome, err, client.gets, client.puts)
		}
	}
}

type markerTestClient struct {
	t          *testing.T
	gets, puts int
	get        func() (*awss3.GetObjectOutput, error)
	put        func(*awss3.PutObjectInput) error
}

func (c *markerTestClient) GetObject(_ context.Context, input *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	c.t.Helper()
	c.gets++
	c.requireDestination(input.Bucket, input.Key)
	if c.get == nil {
		c.t.Fatal("unexpected marker GET")
	}
	return c.get()
}

func (c *markerTestClient) PutObject(_ context.Context, input *awss3.PutObjectInput, _ ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
	c.t.Helper()
	c.puts++
	c.requireDestination(input.Bucket, input.Key)
	if c.put == nil {
		c.t.Fatal("unexpected marker PUT")
	}
	return &awss3.PutObjectOutput{}, c.put(input)
}

func (c *markerTestClient) requireDestination(bucket, key *string) {
	c.t.Helper()
	if aws.ToString(bucket) != "bucket" || aws.ToString(key) != "chain/state-sync/latest.txt" {
		c.t.Fatalf("unexpected marker destination bucket=%q key=%q", aws.ToString(bucket), aws.ToString(key))
	}
}

func markerRead(height, etag string) *awss3.GetObjectOutput {
	return &awss3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(height)), ETag: aws.String(etag)}
}

func markerAPIError(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: "injected S3 failure"}
}

type markerFailingBody struct {
	io.Reader
	readError  error
	closeError error
	closed     bool
}

func (b *markerFailingBody) Read(p []byte) (int, error) {
	if b.readError != nil {
		return 0, b.readError
	}
	return b.Reader.Read(p)
}

func (b *markerFailingBody) Close() error {
	b.closed = true
	return b.closeError
}
