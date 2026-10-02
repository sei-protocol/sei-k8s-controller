package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/sei-protocol/seilog"

	"github.com/sei-protocol/sei-k8s-controller/sidecar/engine"
)

var evmDigestLog = seilog.NewLogger("seictl", "task", "evm-digest")

// DefaultSeiDBBin is where the sidecar image keeps the seidb binary.
const DefaultSeiDBBin = "/usr/bin/seidb"

// DefaultEVMDigestOpenMode is the --memiavl-open-mode used when the task omits
// one: the changelog overlay scan, which is the only mode fast enough to run
// continuously on a live node.
const DefaultEVMDigestOpenMode = "changelog"

// evmDigestParams is the evm-digest task's typed params.
type evmDigestParams struct {
	Height   int64  `json:"height"`
	Backend  string `json:"backend"`
	OpenMode string `json:"openMode,omitempty"`
}

// EVMDigestBackends accepted by the task, matching
// `seidb evm-logical-digest --backend`.
const (
	evmDigestBackendMemiavl   = "memiavl"
	evmDigestBackendComposite = "composite"
)

// EVMDigester runs `seidb evm-logical-digest` against the node's own store
// directories and returns the report JSON object as the task result.
//
// Store layouts differ by node age (see the FlatKV migration runbook's
// "Set node paths" check): memIAVL lives under data/committer.db on newer
// nodes and data/state_commit/memiavl on older ones; FlatKV likewise under
// data/flatkv or data/state_commit/flatkv. The handler resolves the layout
// itself so callers supply only the height and backend.
//
// Scans are serialized on a per-sidecar mutex: a live scan reads the store
// while seid writes it, and two concurrent scans on one node multiply the IO
// and memory load the node must absorb.
type EVMDigester struct {
	homeDir  string
	seidbBin string
	mu       sync.Mutex
	// run executes seidb; a test seam. Defaults to exec.CommandContext.
	run func(ctx context.Context, bin string, args ...string) (stdout, stderr []byte, err error)
}

// NewEVMDigester builds an EVMDigester rooted at homeDir. The binary path
// comes from SEI_SEIDB_BIN when set, else the image's default.
func NewEVMDigester(homeDir string) *EVMDigester {
	bin := os.Getenv("SEI_SEIDB_BIN")
	if bin == "" {
		bin = DefaultSeiDBBin
	}
	return &EVMDigester{homeDir: homeDir, seidbBin: bin}
}

// Handler returns an engine.TaskHandler for the evm-digest task type.
func (d *EVMDigester) Handler() engine.TaskHandler {
	return engine.TypedHandlerWithResult(func(ctx context.Context, params evmDigestParams) (json.RawMessage, error) {
		return d.digest(ctx, params)
	})
}

func (d *EVMDigester) digest(ctx context.Context, params evmDigestParams) (json.RawMessage, error) {
	if params.Height <= 0 {
		return nil, errors.New("evm-digest: height required (must be > 0)")
	}
	openMode := params.OpenMode
	if openMode == "" {
		openMode = DefaultEVMDigestOpenMode
	}

	args := []string{"evm-logical-digest", "--memiavl-open-mode", openMode,
		"--height", fmt.Sprint(params.Height), "--json"}
	switch params.Backend {
	case evmDigestBackendMemiavl:
		dir, err := d.memiavlDir()
		if err != nil {
			return nil, err
		}
		args = append(args, "--backend", "memiavl", "--db-dir", dir)
	case evmDigestBackendComposite:
		flatkvDir, err := d.flatkvDir()
		if err != nil {
			return nil, err
		}
		memiavlDir, err := d.memiavlDir()
		if err != nil {
			return nil, err
		}
		args = append(args, "--backend", "composite",
			"--flatkv-dir", flatkvDir, "--memiavl-dir", memiavlDir)
	default:
		return nil, fmt.Errorf("evm-digest: backend must be %s or %s",
			evmDigestBackendMemiavl, evmDigestBackendComposite)
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	evmDigestLog.Info("starting evm digest scan", "height", params.Height,
		"backend", params.Backend, "openMode", openMode)
	run := d.run
	if run == nil {
		run = execSeiDB
	}
	stdout, stderr, err := run(ctx, d.seidbBin, args...)
	if err != nil {
		return nil, fmt.Errorf("evm-digest: seidb failed: %w%s", err, stderrTail(stderr))
	}

	report, err := parseEVMDigestReport(stdout)
	if err != nil {
		return nil, fmt.Errorf("evm-digest: %w", err)
	}
	evmDigestLog.Info("evm digest scan completed",
		"height", params.Height, "backend", params.Backend,
		"version", report.Version, "count", report.Final.Count)
	return json.RawMessage(stdout), nil
}

// evmDigestReport is the subset of the seidb report the comparison reads.
type evmDigestReport struct {
	Version int64 `json:"version"`
	Final   struct {
		Count  uint64 `json:"count"`
		Digest string `json:"digest"`
	} `json:"final"`
}

// parseEVMDigestReport checks that stdout carries one complete JSON report
// with the fields the cross-node comparison needs.
func parseEVMDigestReport(stdout []byte) (*evmDigestReport, error) {
	var report evmDigestReport
	if err := json.Unmarshal(stdout, &report); err != nil {
		return nil, fmt.Errorf("seidb output is not a JSON report: %w", err)
	}
	if report.Version == 0 {
		return nil, errors.New("seidb report has no version")
	}
	if report.Final.Digest == "" {
		return nil, errors.New("seidb report has no final.digest")
	}
	return &report, nil
}

// memiavlDir resolves the node's memIAVL store dir under data/.
func (d *EVMDigester) memiavlDir() (string, error) {
	dataDir := filepath.Join(d.homeDir, "data")
	if dirExists(filepath.Join(dataDir, "committer.db", "current")) {
		return filepath.Join(dataDir, "committer.db"), nil
	}
	dir := filepath.Join(dataDir, "state_commit", "memiavl")
	if dirExists(dir) {
		return dir, nil
	}
	return "", fmt.Errorf("evm-digest: no memiavl store under %s", dataDir)
}

// flatkvDir resolves the node's FlatKV store dir under data/.
func (d *EVMDigester) flatkvDir() (string, error) {
	dataDir := filepath.Join(d.homeDir, "data")
	if dirExists(filepath.Join(dataDir, "flatkv", "current")) {
		return filepath.Join(dataDir, "flatkv"), nil
	}
	dir := filepath.Join(dataDir, "state_commit", "flatkv")
	if dirExists(dir) {
		return dir, nil
	}
	return "", fmt.Errorf("evm-digest: no flatkv store under %s (node not migrating?)", dataDir)
}

// stderrTail keeps the last bytes of a failed seidb run's stderr for the task
// error without unbounding it.
func stderrTail(stderr []byte) string {
	const maxTail = 2048
	trimmed := bytes.TrimSpace(stderr)
	if len(trimmed) == 0 {
		return ""
	}
	if len(trimmed) > maxTail {
		trimmed = trimmed[len(trimmed)-maxTail:]
	}
	return ": " + string(trimmed)
}

// execSeiDB runs the seidb binary, capturing stdout and stderr separately.
func execSeiDB(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// dirExists reports whether path is an existing directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
