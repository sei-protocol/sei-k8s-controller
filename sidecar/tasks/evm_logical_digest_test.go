package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire"
)

const testDigestReport = `{"backend":"memiavl","requested_height":100,"version":100,` +
	`"account":{"count":3,"digest":"aa"},"code":{"count":1,"digest":"bb"},` +
	`"storage":{"count":5,"digest":"cc"},"misc":{"count":0,"digest":"00"},` +
	`"final":{"count":9,"digest":"ff"}}` + "\n"

func newTestDigester(t *testing.T, home string, run func(ctx context.Context, args []string) ([]byte, error)) *EVMLogicalDigester {
	t.Helper()
	d := NewEVMLogicalDigester(home, "/nonexistent/seidb")
	d.run = run
	return d
}

func runDigest(d *EVMLogicalDigester, req EVMLogicalDigestRequest) (*wire.EVMLogicalDigestResult, error) {
	params, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(params, &m); err != nil {
		return nil, err
	}
	raw, err := d.Handler()(context.Background(), m)
	if err != nil {
		return nil, err
	}
	var res wire.EVMLogicalDigestResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func TestEVMLogicalDigest_MemIAVLArgsAndResult(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, "data", "state_commit", "memiavl", "current"), "")
	var got []string
	d := newTestDigester(t, home, func(_ context.Context, args []string) ([]byte, error) {
		got = args
		return []byte(testDigestReport), nil
	})

	res, err := runDigest(d, EVMLogicalDigestRequest{Backend: wire.EVMDigestMemIAVL, Height: 100})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	want := []string{"evm-logical-digest", "--backend", "memiavl", "--memiavl-open-mode", "changelog",
		"--db-dir", filepath.Join(home, "data", "state_commit", "memiavl"), "--height", "100", "--json"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", got, want)
	}
	if res.Version != 100 || res.RequestedHeight != 100 || res.Backend != wire.EVMDigestMemIAVL {
		t.Errorf("result header = %+v", res)
	}
	if res.Final != (wire.EVMDigestBucket{Count: 9, Digest: "ff"}) || res.Storage.Count != 5 {
		t.Errorf("result buckets = %+v", res)
	}
}

func TestEVMLogicalDigest_CompositePrefersLegacyDirs(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, "data", "committer.db", "current"), "")
	mustWrite(t, filepath.Join(home, "data", "state_commit", "memiavl", "current"), "")
	mustWrite(t, filepath.Join(home, "data", "flatkv", "current"), "")
	var got []string
	d := newTestDigester(t, home, func(_ context.Context, args []string) ([]byte, error) {
		got = args
		return []byte(testDigestReport), nil
	})

	if _, err := runDigest(d, EVMLogicalDigestRequest{Backend: wire.EVMDigestComposite, Height: 100}); err != nil {
		t.Fatalf("digest: %v", err)
	}
	want := []string{"evm-logical-digest", "--backend", "composite", "--memiavl-open-mode", "changelog",
		"--flatkv-dir", filepath.Join(home, "data", "flatkv"),
		"--memiavl-dir", filepath.Join(home, "data", "committer.db"), "--height", "100", "--json"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", got, want)
	}
}

func TestEVMLogicalDigest_Rejects(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, "data", "state_commit", "memiavl", "current"), "")
	ok := func(context.Context, []string) ([]byte, error) { return []byte(testDigestReport), nil }
	cases := map[string]struct {
		req EVMLogicalDigestRequest
		run func(context.Context, []string) ([]byte, error)
	}{
		"unknown backend":     {EVMLogicalDigestRequest{Backend: "flatkv", Height: 100}, ok},
		"zero height":         {EVMLogicalDigestRequest{Backend: wire.EVMDigestMemIAVL}, ok},
		"composite no flatkv": {EVMLogicalDigestRequest{Backend: wire.EVMDigestComposite, Height: 100}, ok},
		"seidb fails": {EVMLogicalDigestRequest{Backend: wire.EVMDigestMemIAVL, Height: 100},
			func(context.Context, []string) ([]byte, error) { return nil, errors.New("exit status 1: no snapshot") }},
		"log line on stdout": {EVMLogicalDigestRequest{Backend: wire.EVMDigestMemIAVL, Height: 100},
			func(context.Context, []string) ([]byte, error) { return []byte(testDigestReport + "ERR boom\n"), nil }},
		"no final digest": {EVMLogicalDigestRequest{Backend: wire.EVMDigestMemIAVL, Height: 100},
			func(context.Context, []string) ([]byte, error) { return []byte(`{"version":100}`), nil }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := runDigest(newTestDigester(t, home, tc.run), tc.req); err == nil {
				t.Fatal("digest succeeded, want error")
			}
		})
	}
}

func TestEVMLogicalDigest_SerializesScans(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, "data", "state_commit", "memiavl", "current"), "")
	var running, maxRunning atomic.Int32
	release := make(chan struct{})
	d := newTestDigester(t, home, func(context.Context, []string) ([]byte, error) {
		n := running.Add(1)
		if n > maxRunning.Load() {
			maxRunning.Store(n)
		}
		<-release
		running.Add(-1)
		return []byte(testDigestReport), nil
	})

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := runDigest(d, EVMLogicalDigestRequest{Backend: wire.EVMDigestMemIAVL, Height: 100})
			errs <- err
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("digest: %v", err)
		}
	}
	if maxRunning.Load() != 1 {
		t.Errorf("max concurrent scans = %d, want 1", maxRunning.Load())
	}
}

func TestEVMLogicalDigest_WaitingScanHonorsCancel(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, "data", "state_commit", "memiavl", "current"), "")
	d := newTestDigester(t, home, func(context.Context, []string) ([]byte, error) { return []byte(testDigestReport), nil })
	d.slot <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.digest(ctx, EVMLogicalDigestRequest{Backend: wire.EVMDigestMemIAVL, Height: 100}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 4}
	_, _ = b.Write([]byte("abc"))
	_, _ = b.Write([]byte("defg"))
	if b.String() != "defg" {
		t.Errorf("tail = %q, want defg", b.String())
	}
}
