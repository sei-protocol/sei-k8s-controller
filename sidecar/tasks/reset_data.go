package tasks

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/sei-protocol/seilog"

	"github.com/sei-protocol/sei-k8s-controller/sidecar/engine"
	"github.com/sei-protocol/sei-k8s-controller/sidecar/rpc"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/tomlpatch"
)

var resetDataLog = seilog.NewLogger("seictl", "task", "reset-data")

// privValidatorStateFile is CometBFT's last-sign-state file. It lives inside
// data/ (unlike node_key.json / priv_validator_key.json, which live in config/
// and are therefore outside the wipe), so the wipe skips it by name: it is the
// validator's double-sign guard, and it must survive every reset.
const privValidatorStateFile = "priv_validator_state.json"

// defaultPrivValidatorStatePath is the state-file path, relative to the home
// root, that the wipe knows how to keep. A config.toml that moves the file
// elsewhere makes the reset refuse rather than guess.
const defaultPrivValidatorStatePath = "data/" + privValidatorStateFile

// emptyPrivValidatorState is the zero last-sign-state seid needs to start when
// no sign state exists yet: height is a JSON string, round and step are
// numbers. It is written only when the file is absent, never over one.
const emptyPrivValidatorState = `{"height":"0","round":0,"step":0}` + "\n"

// dataUserProcesses are the process names (as /proc/<pid>/comm reports them)
// that read or write the data directory. A wipe under any of them corrupts what
// it is doing, or deletes what it is about to read.
var dataUserProcesses = []string{"seid", "seidb"}

// ResetDataResult is the reset-data task's structured result. WipedBytes is the
// pre-wipe on-disk size of data/ (regular files only), surfaced so the
// workflow's hold event can report how much was cleared. It is -1 when the
// measurement failed: the count is observability, never a gate, so a failed
// measurement does not block the reset. Symlinked entries are counted by their
// own (link) size rather than their target, so a data dir that uses symlinks
// may undercount — acceptable for an observability figure.
type ResetDataResult struct {
	WipedBytes int64 `json:"wipedBytes"`
}

// ResetDataer clears the chain data directory for a re-bootstrap. The wipe is
// scoped to <homeDir>/data/ and nothing else: the home root holds config/ (node
// identity), the sidecar's task ledger (sidecar.db — the database that resumes
// this very wipe after a crash), and the hold sentinel/markers. Wiping the home
// root would destroy the machinery mid-flight; this is the design's most
// important correctness rule.
//
// The sign state inside data/ survives: the wipe never deletes it, so a crash
// part-way through cannot leave a re-run that finds no file and writes a zero
// state over a validator's last signed height.
//
// The reset needs no atomicity of its own. A partially deleted data directory
// is only dangerous if seid starts on it, and the node hold guarantees it does
// not. As defense-in-depth the handler refuses to run while seid's local RPC is
// serving (the node is not actually held), while any seid or seidb process
// runs in the pod (an operator's rollback or a digest scan), and when
// config.toml moves the sign state to a path the wipe does not know. It is
// content-idempotent: an already-wiped directory is success.
type ResetDataer struct {
	homeDir string
	probeUp func(ctx context.Context) bool
	// measure returns the pre-wipe size of data/. A test seam; defaults to
	// dirSize when nil.
	measure func(dir string) (int64, error)
	// dataUsers returns the names of running processes that use the data
	// directory. A test seam; defaults to findDataUsers when nil.
	dataUsers func() ([]string, error)
}

// NewResetDataer builds a ResetDataer rooted at homeDir with the real local-RPC
// serving probe.
func NewResetDataer(homeDir string) *ResetDataer {
	statusClient := rpc.NewStatusClient("", nil)
	return &ResetDataer{
		homeDir:   homeDir,
		probeUp:   func(ctx context.Context) bool { return seidRPCUp(ctx, statusClient) },
		measure:   dirSize,
		dataUsers: findDataUsers,
	}
}

// Handler returns an engine.TaskHandler for the reset-data task type. Params
// are empty; the result carries the pre-wipe byte count.
func (d *ResetDataer) Handler() engine.TaskHandler {
	return engine.TypedHandlerWithResult(func(ctx context.Context, _ struct{}) (ResetDataResult, error) {
		return d.reset(ctx)
	})
}

func (d *ResetDataer) reset(ctx context.Context) (ResetDataResult, error) {
	// Defense-in-depth: a serving RPC means seid is running, so the node is not
	// held and a wipe would race a live process. Refuse rather than wipe under
	// it (mirrors restart-seid's refusal to report a stop that did not happen).
	if d.probeUp(ctx) {
		return ResetDataResult{}, fmt.Errorf("reset-data: seid RPC is serving; node is not held — refusing to wipe a live data directory")
	}
	if err := d.refuseDataUsers(); err != nil {
		return ResetDataResult{}, err
	}
	if err := d.refuseMovedSignState(); err != nil {
		return ResetDataResult{}, err
	}

	dataDir := filepath.Join(d.homeDir, "data")

	// Measurement is observability only — never let it gate the wipe. On any
	// non-ENOENT failure, log and proceed with an unknown (-1) size.
	measure := d.measure
	if measure == nil {
		measure = dirSize
	}
	size, err := measure(dataDir)
	if err != nil {
		resetDataLog.Warn("measuring data dir failed; proceeding with unknown size", "dir", dataDir, "err", err)
		size = -1
	}

	if err := wipeDirContents(dataDir, privValidatorStateFile); err != nil {
		return ResetDataResult{}, fmt.Errorf("reset-data: wiping %s: %w", dataDir, err)
	}

	// Recreate data/ (the wipe may have removed it if it was empty of anything
	// but itself) and write the zero sign state only where none exists: a node
	// that never signed needs it to start, and a validator keeps its own.
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return ResetDataResult{}, fmt.Errorf("reset-data: recreating %s: %w", dataDir, err)
	}
	statePath := filepath.Join(dataDir, privValidatorStateFile)
	switch _, err := os.Stat(statePath); {
	case err == nil:
		resetDataLog.Info("kept existing sign state", "path", statePath)
	case os.IsNotExist(err):
		if err := writeFileSynced(statePath, []byte(emptyPrivValidatorState), 0o600); err != nil {
			return ResetDataResult{}, fmt.Errorf("reset-data: writing %s: %w", statePath, err)
		}
	default:
		return ResetDataResult{}, fmt.Errorf("reset-data: checking %s: %w", statePath, err)
	}

	// Clear the state-sync completion marker (home root, outside data/) so the
	// downstream configure-state-sync reruns instead of short-circuiting.
	markerPath := filepath.Join(d.homeDir, stateSyncMarkerFile)
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return ResetDataResult{}, fmt.Errorf("reset-data: removing marker %s: %w", markerPath, err)
	}

	resetDataLog.Info("data directory reset", "dir", dataDir, "wipedBytes", size)
	return ResetDataResult{WipedBytes: size}, nil
}

// refuseDataUsers fails the reset while a seid or seidb process runs anywhere
// in the pod. The pod shares one PID namespace, so the sidecar sees an
// operator's `seid rollback` in the seid container and its own digest scans.
func (d *ResetDataer) refuseDataUsers() error {
	find := d.dataUsers
	if find == nil {
		find = findDataUsers
	}
	users, err := find()
	if err != nil {
		return fmt.Errorf("reset-data: listing processes that use the data directory: %w", err)
	}
	if len(users) > 0 {
		return fmt.Errorf("reset-data: %s running in the pod — refusing to wipe data it is using; finish or stop it first",
			strings.Join(users, ", "))
	}
	return nil
}

// refuseMovedSignState fails the reset when config.toml points the sign state
// anywhere but data/priv_validator_state.json. The wipe keeps the sign state by
// name; under another path inside data/ it would delete it, and seid would
// start on a zero state. Both spellings count: CometBFT's
// [priv-validator] state-file and sei-config's [priv_validator] state_file. A
// value that is present but not a string is a refusal too.
func (d *ResetDataer) refuseMovedSignState() error {
	configPath := filepath.Join(d.homeDir, "config", "config.toml")
	doc, err := tomlpatch.ReadTOML(configPath)
	if err != nil {
		return fmt.Errorf("reset-data: reading %s to locate the sign state: %w", configPath, err)
	}
	for _, sectionName := range []string{"priv-validator", "priv_validator"} {
		raw, present := doc[sectionName]
		if !present {
			continue
		}
		section, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("reset-data: config.toml [%s] is not a table; refusing rather than guess where the sign state is", sectionName)
		}
		for _, key := range []string{"state-file", "state_file"} {
			value, present := section[key]
			if !present {
				continue
			}
			configured, ok := value.(string)
			if !ok {
				return fmt.Errorf("reset-data: config.toml [%s] %s is not a string; refusing rather than guess where the sign state is", sectionName, key)
			}
			if configured == "" || filepath.Clean(configured) == defaultPrivValidatorStatePath ||
				filepath.Clean(configured) == filepath.Join(d.homeDir, defaultPrivValidatorStatePath) {
				continue
			}
			return fmt.Errorf("reset-data: config.toml sets [%s] %s = %q; the reset keeps only %s, so it refuses rather than risk deleting the sign state",
				sectionName, key, configured, defaultPrivValidatorStatePath)
		}
	}
	return nil
}

// findDataUsers scans /proc for processes whose comm is in dataUserProcesses.
// comm, not argv[0]: the seid container's start gate is a bash loop whose
// script text names seid, and that must not count.
func findDataUsers() ([]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("reading /proc: %w", err)
	}
	var users []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err != nil {
			continue // exited between ReadDir and ReadFile
		}
		name := strings.TrimSpace(string(comm))
		if slices.Contains(dataUserProcesses, name) {
			users = append(users, fmt.Sprintf("%s (pid %d)", name, pid))
		}
	}
	return users, nil
}

// writeFileSynced writes content and fsyncs it before returning, so the zero
// sign state is on disk before the reset reports success.
func writeFileSynced(path string, content []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// wipeDirContents removes every entry under dir except the top-level entries
// named in keep, leaving dir itself. A missing dir is success
// (content-idempotent: already-empty is the goal state), and a concurrent peer
// removing an entry first (ENOENT) is tolerated so a rehydrated re-run cannot
// fail on a half-wiped tree.
func wipeDirContents(dir string, keep ...string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if slices.Contains(keep, e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %s: %w", p, err)
		}
	}
	return nil
}

// dirSize sums the on-disk size of regular files under dir. A missing dir is
// zero. Errors from entries that vanish mid-walk (ENOENT) are ignored — the
// count is observability, not a correctness signal.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return total, err
}
