package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// newResetDataer builds a ResetDataer over homeDir whose RPC probe reports the
// given serving state (true = seid up, which the reset must refuse).
func newResetDataer(homeDir string, rpcUp bool) *ResetDataer {
	return &ResetDataer{
		homeDir:   homeDir,
		probeUp:   func(context.Context) bool { return rpcUp },
		dataUsers: func() ([]string, error) { return nil, nil },
	}
}

// seedHome lays out a realistic home root: data/ with chain files, config/ with
// identity, the sidecar task ledger, and the state-sync marker.
func seedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()

	mustWrite(t, filepath.Join(home, "data", "blockstore.db", "000001.log"), "blocks")
	mustWrite(t, filepath.Join(home, "data", "application.db", "CURRENT"), "app")
	mustWrite(t, filepath.Join(home, "data", privValidatorStateFile), seededSignState)
	mustWrite(t, filepath.Join(home, "config", "config.toml"), "[p2p]\npex = true\n")
	mustWrite(t, filepath.Join(home, "config", "node_key.json"), "nodekey")
	mustWrite(t, filepath.Join(home, "config", "priv_validator_key.json"), "conskey")
	mustWrite(t, filepath.Join(home, "sidecar.db"), "ledger")
	mustWrite(t, filepath.Join(home, stateSyncMarkerFile), "")
	return home
}

// seededSignState is a validator sign state at a non-zero height, with the
// signature fields a real FilePV writes.
const seededSignState = `{"height":"987","round":2,"step":3,"signature":"c2ln","signbytes":"AQID"}`

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func runReset(t *testing.T, d *ResetDataer) ResetDataResult {
	t.Helper()
	raw, err := d.Handler()(context.Background(), nil)
	if err != nil {
		t.Fatalf("reset-data: %v", err)
	}
	var res ResetDataResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("decoding result: %v", err)
		}
	}
	return res
}

func TestResetData_WipesDataPreservesHomeRoot(t *testing.T) {
	home := seedHome(t)
	res := runReset(t, newResetDataer(home, false))

	// data/ chain files gone.
	if exists(filepath.Join(home, "data", "blockstore.db")) {
		t.Error("blockstore.db survived the wipe")
	}
	if exists(filepath.Join(home, "data", "application.db")) {
		t.Error("application.db survived the wipe")
	}
	// Home-root siblings untouched — the design's core correctness rule.
	for _, p := range []string{
		filepath.Join(home, "config", "config.toml"),
		filepath.Join(home, "config", "node_key.json"),
		filepath.Join(home, "config", "priv_validator_key.json"),
		filepath.Join(home, "sidecar.db"),
	} {
		if !exists(p) {
			t.Errorf("home-root file destroyed by wipe: %s", p)
		}
	}
	if res.WipedBytes == 0 {
		t.Error("expected non-zero wipedBytes for a seeded data dir")
	}
}

func TestResetData_RemovesStateSyncMarker(t *testing.T) {
	home := seedHome(t)
	runReset(t, newResetDataer(home, false))
	if exists(filepath.Join(home, stateSyncMarkerFile)) {
		t.Error("state-sync marker survived the reset")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(got)
}

// 009 Req 4.1 / SC-007: the sign state survives the wipe byte for byte, and
// the keys in config/ are untouched.
func TestResetData_KeepsSignStateAndKeys(t *testing.T) {
	home := seedHome(t)
	runReset(t, newResetDataer(home, false))

	if got := readFile(t, filepath.Join(home, "data", privValidatorStateFile)); got != seededSignState {
		t.Errorf("priv_validator_state = %q, want the seeded %q", got, seededSignState)
	}
	if got := readFile(t, filepath.Join(home, "config", "priv_validator_key.json")); got != "conskey" {
		t.Errorf("priv_validator_key.json = %q, want untouched", got)
	}
	if got := readFile(t, filepath.Join(home, "config", "node_key.json")); got != "nodekey" {
		t.Errorf("node_key.json = %q, want untouched", got)
	}
}

// 009 Req 4.2: a node with no sign state gets the zero state seid needs.
func TestResetData_WritesZeroSignStateWhenAbsent(t *testing.T) {
	home := seedHome(t)
	if err := os.Remove(filepath.Join(home, "data", privValidatorStateFile)); err != nil {
		t.Fatal(err)
	}
	runReset(t, newResetDataer(home, false))

	if got := readFile(t, filepath.Join(home, "data", privValidatorStateFile)); got != emptyPrivValidatorState {
		t.Errorf("priv_validator_state = %q, want %q", got, emptyPrivValidatorState)
	}
}

// 009 Req 4.1, User Story 3 scenario 2: a reset that stopped part-way (the
// engine rehydrates a task left running across a crash) still finds the sign
// state on the re-run. Deleting and rewriting it instead would let a crash in
// between turn the re-run's "absent" branch into a zero state.
func TestResetData_RerunAfterPartialWipeKeepsSignState(t *testing.T) {
	home := seedHome(t)
	// Simulate the first run dying after it removed one chain directory.
	if err := os.RemoveAll(filepath.Join(home, "data", "blockstore.db")); err != nil {
		t.Fatal(err)
	}
	runReset(t, newResetDataer(home, false))

	if got := readFile(t, filepath.Join(home, "data", privValidatorStateFile)); got != seededSignState {
		t.Errorf("priv_validator_state after re-run = %q, want the seeded %q", got, seededSignState)
	}
	if exists(filepath.Join(home, "data", "application.db")) {
		t.Error("application.db survived the re-run")
	}
}

func TestResetData_IdempotentOverAlreadyWiped(t *testing.T) {
	home := seedHome(t)
	runReset(t, newResetDataer(home, false))

	// Second run over the already-wiped dir: success, and the sign state is
	// still the seeded one.
	res := runReset(t, newResetDataer(home, false))
	if got := readFile(t, filepath.Join(home, "data", privValidatorStateFile)); got != seededSignState {
		t.Errorf("priv_validator_state after re-run = %q, want %q", got, seededSignState)
	}
	// Only the small state file remains, so the second wipe measures little.
	if res.WipedBytes >= int64(len(seededSignState))+64 {
		t.Errorf("second-run wipedBytes unexpectedly large: %d", res.WipedBytes)
	}
}

// 009 Req 4.4: a state-file the wipe does not know makes the reset refuse,
// and the data stays as it was. The default path, relative or absolute, is
// accepted.
func TestResetData_StateFilePath(t *testing.T) {
	cases := []struct {
		name      string
		stateFile func(home string) string
		refuse    bool
	}{
		{"moved inside data", func(string) string { return "data/signing/state.json" }, true},
		{"moved outside data", func(string) string { return "/var/sei/pvs.json" }, true},
		{"default relative", func(string) string { return "data/priv_validator_state.json" }, false},
		{"default absolute", func(home string) string { return filepath.Join(home, "data", privValidatorStateFile) }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := seedHome(t)
			mustWrite(t, filepath.Join(home, "config", "config.toml"),
				fmt.Sprintf("[priv-validator]\nstate-file = %q\n", tc.stateFile(home)))

			_, err := newResetDataer(home, false).Handler()(context.Background(), nil)
			if tc.refuse != (err != nil) {
				t.Fatalf("refuse = %v, err = %v", tc.refuse, err)
			}
			if tc.refuse && !exists(filepath.Join(home, "data", "blockstore.db")) {
				t.Error("blockstore.db was wiped despite the refusal")
			}
		})
	}
}

// An unreadable config.toml means the reset cannot tell where the sign state
// is, so it refuses rather than guess.
func TestResetData_RefusesUnreadableConfig(t *testing.T) {
	home := seedHome(t)
	mustWrite(t, filepath.Join(home, "config", "config.toml"), "not = [toml")

	if _, err := newResetDataer(home, false).Handler()(context.Background(), nil); err == nil {
		t.Fatal("expected refusal on an unparsable config.toml")
	}
	if !exists(filepath.Join(home, "data", "blockstore.db")) {
		t.Error("blockstore.db was wiped despite the refusal")
	}
}

// 009 Req 4.5: an operator's seid rollback or a seidb scan blocks the wipe.
func TestResetData_RefusesWhileDataUserRuns(t *testing.T) {
	home := seedHome(t)
	d := newResetDataer(home, false)
	d.dataUsers = func() ([]string, error) { return []string{"seidb (pid 42)"}, nil }

	_, err := d.Handler()(context.Background(), nil)
	if err == nil {
		t.Fatal("expected refusal while seidb runs")
	}
	if !exists(filepath.Join(home, "data", "blockstore.db")) {
		t.Error("blockstore.db was wiped despite the refusal")
	}
}

// A process listing that fails is a refusal too: the reset cannot prove the
// data is unused.
func TestResetData_RefusesWhenProcessListFails(t *testing.T) {
	home := seedHome(t)
	d := newResetDataer(home, false)
	d.dataUsers = func() ([]string, error) { return nil, fmt.Errorf("reading /proc: denied") }

	if _, err := d.Handler()(context.Background(), nil); err == nil {
		t.Fatal("expected refusal when the process list fails")
	}
}

func TestResetData_MissingDataDirIsSuccess(t *testing.T) {
	home := t.TempDir() // no data/ at all
	res := runReset(t, newResetDataer(home, false))
	if res.WipedBytes != 0 {
		t.Errorf("expected 0 wipedBytes for absent data dir, got %d", res.WipedBytes)
	}
	if !exists(filepath.Join(home, "data", privValidatorStateFile)) {
		t.Error("expected a fresh priv_validator_state to be created")
	}
}

func TestResetData_MeasurementFailureYieldsUnknownSize(t *testing.T) {
	home := seedHome(t)
	d := newResetDataer(home, false)
	d.measure = func(string) (int64, error) { return 0, fmt.Errorf("simulated measurement failure") }

	res := runReset(t, d)

	if res.WipedBytes != -1 {
		t.Errorf("WipedBytes = %d, want -1 (unknown) on measurement failure", res.WipedBytes)
	}
	// Measurement must not gate the wipe: the reset still cleared data/ and
	// wrote a fresh sign-state.
	if exists(filepath.Join(home, "data", "blockstore.db")) {
		t.Error("data not wiped after measurement failure — measurement gated the reset")
	}
	if !exists(filepath.Join(home, "data", privValidatorStateFile)) {
		t.Error("fresh priv_validator_state not written after measurement failure")
	}
}

func TestResetData_RefusesWhenRPCServing(t *testing.T) {
	home := seedHome(t)
	_, err := newResetDataer(home, true).Handler()(context.Background(), nil)
	if err == nil {
		t.Fatal("expected refusal when seid RPC is serving")
	}
	// Data must be untouched on refusal.
	if !exists(filepath.Join(home, "data", "blockstore.db")) {
		t.Error("blockstore.db was wiped despite RPC-serving refusal")
	}
}
