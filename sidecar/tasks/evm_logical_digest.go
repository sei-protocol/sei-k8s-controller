package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sei-protocol/seilog"

	"github.com/sei-protocol/sei-k8s-controller/sidecar/engine"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire"
)

var evmDigestLog = seilog.NewLogger("seictl", "task", "evm-logical-digest")

// DefaultSeidbPath is where the sidecar image installs seidb.
const DefaultSeidbPath = "/usr/bin/seidb"

// evmDigestStderrTail bounds how much of seidb's stderr a failed scan carries
// into its task error.
const evmDigestStderrTail = 4096

// EVMLogicalDigestRequest is the evm-logical-digest task's params.
type EVMLogicalDigestRequest struct {
	Backend wire.EVMDigestBackend `json:"backend"`
	Height  int64                 `json:"height"`
}

// seidbDigestReport is the subset of `seidb evm-logical-digest --json` the
// task reads.
type seidbDigestReport struct {
	Version int64                `json:"version"`
	Account wire.EVMDigestBucket `json:"account"`
	Code    wire.EVMDigestBucket `json:"code"`
	Storage wire.EVMDigestBucket `json:"storage"`
	Misc    wire.EVMDigestBucket `json:"misc"`
	Final   wire.EVMDigestBucket `json:"final"`
}

// EVMLogicalDigester runs `seidb evm-logical-digest` against the node's own
// state-commit directories. Scans read every EVM key, so they are serialized:
// a second task waits for the running one rather than competing with it and
// with seid for disk.
type EVMLogicalDigester struct {
	homeDir string
	// run executes seidb with args and returns its stdout. A test seam.
	run  func(ctx context.Context, args []string) ([]byte, error)
	slot chan struct{}
	now  func() time.Time
}

// NewEVMLogicalDigester builds a digester over homeDir that runs the seidb
// binary at seidbPath.
func NewEVMLogicalDigester(homeDir, seidbPath string) *EVMLogicalDigester {
	return &EVMLogicalDigester{
		homeDir: homeDir,
		run:     func(ctx context.Context, args []string) ([]byte, error) { return runSeidb(ctx, seidbPath, args) },
		slot:    make(chan struct{}, 1),
		now:     time.Now,
	}
}

// Handler returns an engine.TaskHandler for the evm-logical-digest task type.
func (d *EVMLogicalDigester) Handler() engine.TaskHandler {
	return engine.TypedHandlerWithResult(func(ctx context.Context, req EVMLogicalDigestRequest) (*wire.EVMLogicalDigestResult, error) {
		return d.digest(ctx, req)
	})
}

func (d *EVMLogicalDigester) digest(ctx context.Context, req EVMLogicalDigestRequest) (*wire.EVMLogicalDigestResult, error) {
	if err := req.Backend.Validate(); err != nil {
		return nil, fmt.Errorf("evm-logical-digest: %w", err)
	}
	if req.Height <= 0 {
		return nil, fmt.Errorf("evm-logical-digest: height must be positive, got %d", req.Height)
	}
	args, err := d.args(req)
	if err != nil {
		return nil, fmt.Errorf("evm-logical-digest: %w", err)
	}

	select {
	case d.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-d.slot }()

	evmDigestLog.Info("scan started", "backend", req.Backend, "height", req.Height)
	start := d.now()
	stdout, err := d.run(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("evm-logical-digest: seidb at height %d: %w", req.Height, err)
	}
	report, err := parseSeidbDigest(stdout)
	if err != nil {
		return nil, fmt.Errorf("evm-logical-digest: %w", err)
	}
	res := &wire.EVMLogicalDigestResult{
		Backend:         req.Backend,
		RequestedHeight: req.Height,
		Version:         report.Version,
		Account:         report.Account,
		Code:            report.Code,
		Storage:         report.Storage,
		Misc:            report.Misc,
		Final:           report.Final,
		DurationSeconds: d.now().Sub(start).Seconds(),
	}
	evmDigestLog.Info("scan finished", "backend", req.Backend, "height", req.Height,
		"version", res.Version, "count", res.Final.Count, "digest", res.Final.Digest,
		"seconds", res.DurationSeconds)
	return res, nil
}

// args builds the seidb command line for req, resolving store directories the
// way seid does (legacy layout first).
func (d *EVMLogicalDigester) args(req EVMLogicalDigestRequest) ([]string, error) {
	memiavlDir, err := existingDir(
		filepath.Join(d.homeDir, "data", "committer.db"),
		filepath.Join(d.homeDir, "data", "state_commit", "memiavl"))
	if err != nil {
		return nil, fmt.Errorf("memiavl dir: %w", err)
	}
	args := []string{"evm-logical-digest", "--backend", string(req.Backend), "--memiavl-open-mode", "changelog"}
	switch req.Backend {
	case wire.EVMDigestMemIAVL:
		args = append(args, "--db-dir", memiavlDir)
	case wire.EVMDigestComposite:
		flatkvDir, err := existingDir(
			filepath.Join(d.homeDir, "data", "flatkv"),
			filepath.Join(d.homeDir, "data", "state_commit", "flatkv"))
		if err != nil {
			return nil, fmt.Errorf("flatkv dir: %w", err)
		}
		args = append(args, "--flatkv-dir", flatkvDir, "--memiavl-dir", memiavlDir)
	}
	return append(args, "--height", strconv.FormatInt(req.Height, 10), "--json"), nil
}

// existingDir returns the first candidate that is a directory.
func existingDir(candidates ...string) (string, error) {
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("none of %s exists", strings.Join(candidates, ", "))
}

// parseSeidbDigest decodes seidb's stdout, which must be exactly one JSON
// object. Anything else means a log line leaked onto stdout and the report
// cannot be trusted to be the whole output.
func parseSeidbDigest(stdout []byte) (seidbDigestReport, error) {
	dec := json.NewDecoder(bytes.NewReader(stdout))
	var report seidbDigestReport
	if err := dec.Decode(&report); err != nil {
		return seidbDigestReport{}, fmt.Errorf("decoding seidb output: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return seidbDigestReport{}, errors.New("seidb output is not a single JSON object")
	}
	if report.Final.Digest == "" {
		return seidbDigestReport{}, errors.New("seidb output has no final digest")
	}
	return report, nil
}

func runSeidb(ctx context.Context, seidbPath string, args []string) ([]byte, error) {
	var stdout bytes.Buffer
	stderr := &tailBuffer{max: evmDigestStderrTail}
	cmd := exec.CommandContext(ctx, seidbPath, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(), "SEI_LOG_OUTPUT=stderr")
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		if tail := strings.TrimSpace(stderr.String()); tail != "" {
			return nil, fmt.Errorf("%w: %s", err, tail)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
