package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testDigestReport = `{"version": 1, "backend": "memiavl", "final": {"count": 7, "digest": "0xabc"}}`

func mustMkdirEVM(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func newTestDigester(t *testing.T, run func(ctx context.Context, bin string, args ...string) ([]byte, []byte, error)) *EVMDigester {
	t.Helper()
	return &EVMDigester{
		homeDir:  t.TempDir(),
		seidbBin: "/fake/seidb",
		run:      run,
	}
}

func okRun(stdout []byte) func(context.Context, string, ...string) ([]byte, []byte, error) {
	return func(context.Context, string, ...string) ([]byte, []byte, error) {
		return stdout, nil, nil
	}
}

func callDigest(t *testing.T, d *EVMDigester, params evmDigestParams) (json.RawMessage, error) {
	t.Helper()
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	return d.Handler()(context.Background(), p)
}

func TestEVMDigest_MemiavlCommitterDBLayout(t *testing.T) {
	d := newTestDigester(t, okRun([]byte(testDigestReport)))
	mustMkdirEVM(t, filepath.Join(d.homeDir, "data", "committer.db", "current"))

	var gotArgs []string
	d.run = func(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
		gotArgs = args
		return []byte(testDigestReport), nil, nil
	}

	out, err := callDigest(t, d, evmDigestParams{Height: 42, Backend: "memiavl"})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{
		"evm-logical-digest",
		"--backend memiavl",
		"--memiavl-open-mode changelog",
		"--height 42",
		"--json",
		"--db-dir " + filepath.Join(d.homeDir, "data", "committer.db"),
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
	var got, want any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(testDigestReport), &want); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
		t.Errorf("result = %v, want the report object %v", got, want)
	}
}

func TestEVMDigest_MemiavlLegacyLayout(t *testing.T) {
	d := newTestDigester(t, okRun([]byte(testDigestReport)))
	legacy := filepath.Join(d.homeDir, "data", "state_commit", "memiavl")
	mustMkdirEVM(t, legacy)

	var gotArgs []string
	d.run = func(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
		gotArgs = args
		return []byte(testDigestReport), nil, nil
	}

	if _, err := callDigest(t, d, evmDigestParams{Height: 42, Backend: "memiavl"}); err != nil {
		t.Fatalf("digest: %v", err)
	}
	if !strings.Contains(strings.Join(gotArgs, " "), "--db-dir "+legacy) {
		t.Errorf("args %q missing --db-dir %s", gotArgs, legacy)
	}
}

func TestEVMDigest_CompositePassesBothDirs(t *testing.T) {
	d := newTestDigester(t, okRun([]byte(testDigestReport)))
	mustMkdirEVM(t, filepath.Join(d.homeDir, "data", "flatkv", "current"))
	mustMkdirEVM(t, filepath.Join(d.homeDir, "data", "state_commit", "memiavl"))

	var gotArgs []string
	d.run = func(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
		gotArgs = args
		return []byte(testDigestReport), nil, nil
	}

	if _, err := callDigest(t, d, evmDigestParams{Height: 10, Backend: "composite"}); err != nil {
		t.Fatalf("digest: %v", err)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{
		"--backend composite",
		"--flatkv-dir " + filepath.Join(d.homeDir, "data", "flatkv"),
		"--memiavl-dir " + filepath.Join(d.homeDir, "data", "state_commit", "memiavl"),
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
}

func TestEVMDigest_OpenModeOverride(t *testing.T) {
	d := newTestDigester(t, okRun([]byte(testDigestReport)))
	mustMkdirEVM(t, filepath.Join(d.homeDir, "data", "state_commit", "memiavl"))

	var gotArgs []string
	d.run = func(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
		gotArgs = args
		return []byte(testDigestReport), nil, nil
	}

	if _, err := callDigest(t, d, evmDigestParams{Height: 10, Backend: "memiavl", OpenMode: "replay"}); err != nil {
		t.Fatalf("digest: %v", err)
	}
	if !strings.Contains(strings.Join(gotArgs, " "), "--memiavl-open-mode replay") {
		t.Errorf("args %q missing open-mode replay", gotArgs)
	}
}

func TestEVMDigest_Errors(t *testing.T) {
	d := newTestDigester(t, okRun([]byte(testDigestReport)))

	if _, err := callDigest(t, d, evmDigestParams{Height: 0, Backend: "memiavl"}); err == nil {
		t.Error("height 0: want error")
	}
	if _, err := callDigest(t, d, evmDigestParams{Height: 1, Backend: "rocksdb"}); err == nil {
		t.Error("bad backend: want error")
	}
	if _, err := callDigest(t, d, evmDigestParams{Height: 1, Backend: "memiavl"}); err == nil {
		t.Error("missing memiavl dir: want error")
	}
	if _, err := callDigest(t, d, evmDigestParams{Height: 1, Backend: "composite"}); err == nil {
		t.Error("missing flatkv dir: want error")
	}
}

func TestEVMDigest_SeidbFailures(t *testing.T) {
	d := newTestDigester(t, nil)
	mustMkdirEVM(t, filepath.Join(d.homeDir, "data", "state_commit", "memiavl"))

	d.run = func(context.Context, string, ...string) ([]byte, []byte, error) {
		return nil, []byte("changelog ends mid-record at chunk 512"), errors.New("exit status 1")
	}
	if _, err := callDigest(t, d, evmDigestParams{Height: 1, Backend: "memiavl"}); err == nil ||
		!strings.Contains(err.Error(), "mid-record") {
		t.Errorf("seidb failure: want stderr in error, got %v", err)
	}

	d.run = okRun([]byte(`not json`))
	if _, err := callDigest(t, d, evmDigestParams{Height: 1, Backend: "memiavl"}); err == nil {
		t.Error("non-JSON stdout: want error")
	}

	d.run = okRun([]byte(`{"version": 1}`))
	if _, err := callDigest(t, d, evmDigestParams{Height: 1, Backend: "memiavl"}); err == nil {
		t.Error("report without final.digest: want error")
	}
}

func TestEVMDigest_SerializesScans(t *testing.T) {
	d := newTestDigester(t, nil)
	mustMkdirEVM(t, filepath.Join(d.homeDir, "data", "state_commit", "memiavl"))

	var inFlight atomic.Int32
	var maxSeen atomic.Int32
	d.run = func(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
		n := inFlight.Add(1)
		for {
			m := maxSeen.Load()
			if n <= m || maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		return []byte(testDigestReport), nil, nil
	}

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := callDigest(t, d, evmDigestParams{Height: 1, Backend: "memiavl"})
			errs <- err
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("digest: %v", err)
		}
	}
	if got := maxSeen.Load(); got != 1 {
		t.Errorf("max concurrent scans = %d, want 1", got)
	}
}

// TestEVMDigest_ExecSeiDB exercises the real exec seam against a shell-double
// so the CommandContext wiring (stdout/stderr split) is covered.
func TestEVMDigest_ExecSeiDB(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "seidb")
	script := "#!/bin/sh\necho '{\"version\":1,\"final\":{\"count\":1,\"digest\":\"0x00\"}}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := execSeiDB(context.Background(), bin, "evm-logical-digest")
	if err != nil {
		t.Fatalf("execSeiDB: %v", err)
	}
	if !strings.Contains(string(stdout), `"digest":"0x00"`) {
		t.Errorf("stdout = %s", stdout)
	}
	if len(stderr) != 0 {
		t.Errorf("stderr = %s, want empty", stderr)
	}
}
