package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sei-protocol/sei-k8s-controller/sidecar/engine"
	seis3 "github.com/sei-protocol/sei-k8s-controller/sidecar/s3"
)

const restoreRecoveryTaskID = "11111111-1111-4111-8111-111111111111"

// Record failures model shutdown after installation. Real SQLite reopening and
// engine rehydration must finish the same task without downloading the store again.
func TestSnapshotRestoreEngineRestartCompletesInstalledTask(t *testing.T) {
	for _, blockedRecord := range []string{SnapshotHeightFile, restoreMarkerFile} {
		t.Run(blockedRecord, func(t *testing.T) {
			home := t.TempDir()
			writeRestoreNeighbors(t, home)
			incoming := createRestoreDiagnosticArchive(t, 1000, 0)
			client := &restoreHookClient{mockTransferClient: &mockTransferClient{responses: map[string][]byte{
				"chain/state-sync/1000.tar.gz": diagnosticChunksFirstArchive(t, incoming.directory),
			}}}
			blocker := filepath.Join(home, blockedRecord)
			if blockedRecord == SnapshotHeightFile {
				if err := os.Mkdir(blocker, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				client.afterDownload = func() {
					if err := os.Mkdir(blocker, 0o755); err != nil {
						t.Error(err)
					}
				}
			}
			restorer := mustNewRestorer(t, home, "bucket", "region", "chain", mockClientFactory(client), mockListerFactory(&mockObjectLister{keys: []string{"chain/state-sync/1000.tar.gz"}}))
			databasePath := filepath.Join(t.TempDir(), "sidecar.db")
			firstStore, err := engine.NewSQLiteStore(databasePath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = firstStore.Close() })
			firstContext, stopFirstEngine := context.WithCancel(t.Context())
			defer stopFirstEngine()
			interrupted := make(chan error, 1)
			handler := restorer.Handler()
			firstEngine := engine.NewEngine(firstContext, map[engine.TaskType]engine.TaskHandler{
				engine.TaskSnapshotRestore: func(ctx context.Context, params map[string]any) (json.RawMessage, error) {
					result, err := handler(ctx, params)
					// Returning an error under engine shutdown leaves the real
					// task row Running, just as an interrupted process does.
					stopFirstEngine()
					interrupted <- err
					return result, err
				},
			}, firstStore)
			// The requested cap differs from the selected height. Recovery must
			// retain the installed height rather than record the cap or rediscover.
			_, err = firstEngine.Submit(engine.Task{ID: restoreRecoveryTaskID, Type: engine.TaskSnapshotRestore, Params: map[string]any{"targetHeight": 1500}})
			if err != nil {
				t.Fatal(err)
			}
			if err := <-interrupted; err == nil || !strings.Contains(err.Error(), "snapshot installed but") {
				t.Fatalf("fixture did not stop after installation: %v", err)
			}
			running, err := firstStore.Get(restoreRecoveryTaskID)
			if err != nil || running == nil || running.Status != engine.TaskStatusRunning || running.Run != 1 {
				t.Fatalf("expected persisted running task: %+v, %v", running, err)
			}
			destination := filepath.Join(home, "data", "snapshots")
			before := captureRestoreTree(t, destination)
			if len(before) == 0 {
				t.Fatal("fixture did not install snapshot files")
			}
			if info, err := os.Lstat(filepath.Join(home, restoreMarkerFile)); err == nil && info.Mode().IsRegular() {
				t.Fatal("fixture published completion before interruption")
			}
			if err := firstStore.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(blocker); err != nil {
				t.Fatal(err)
			}

			// A fresh instance has no receipt or selected S3 key in memory.
			recovered := diagnosticRestorer(t, home, nil)
			var networkCalls atomic.Int32
			recovered.clientFactory = func(context.Context, string) (seis3.TransferClient, error) {
				networkCalls.Add(1)
				return nil, fmt.Errorf("unexpected download client during recovery")
			}
			recovered.listerFactory = func(context.Context, string) (seis3.ObjectLister, error) {
				networkCalls.Add(1)
				return nil, fmt.Errorf("unexpected discovery during recovery")
			}
			reopened, err := engine.NewSQLiteStore(databasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			observed := &restoreRecoveryStore{ResultStore: reopened, terminal: make(chan engine.TaskResult, 1)}
			restarted := engine.NewEngine(t.Context(), map[engine.TaskType]engine.TaskHandler{engine.TaskSnapshotRestore: recovered.Handler()}, observed)
			restarted.RehydrateStaleTasks()
			result := <-observed.terminal
			if result.Status != engine.TaskStatusCompleted || result.Error != "" || result.ID != restoreRecoveryTaskID || result.Run != 1 {
				t.Fatalf("same task did not complete after restart: %+v", result)
			}
			if networkCalls.Load() != 0 {
				t.Fatalf("recovery rediscovered or downloaded: calls=%d", networkCalls.Load())
			}
			if !reflect.DeepEqual(before, captureRestoreTree(t, destination)) {
				t.Fatal("recovery modified the installed snapshot files")
			}
			height, err := os.ReadFile(filepath.Join(home, SnapshotHeightFile))
			if err != nil || string(height) != "1000" || !markerExists(home, restoreMarkerFile) {
				t.Fatalf("recovery did not publish installed height and completion: height=%q error=%v", height, err)
			}
			requireDiagnosticSnapshotMembership(t, home, 1000)
			requireRestoreNeighbors(t, home)
			requireNoRestoreStaging(t, home)
		})
	}
}

type restoreRecoveryStore struct {
	engine.ResultStore
	terminal chan engine.TaskResult
}

func (s *restoreRecoveryStore) Save(result *engine.TaskResult) error {
	if err := s.ResultStore.Save(result); err != nil {
		return err
	}
	if result.Status != engine.TaskStatusRunning {
		s.terminal <- *result
	}
	return nil
}

func TestSnapshotRestoreEngineTaskInstallsAndCompletes(t *testing.T) {
	home := t.TempDir()
	writeRestoreNeighbors(t, home)
	incoming := createRestoreDiagnosticArchive(t, 1000, 0)

	restorer := diagnosticRestorer(t, home, map[string][]byte{"1000": diagnosticChunksFirstArchive(t, incoming.directory)})
	store, err := engine.NewMemoryStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	observed := &restoreRecoveryStore{ResultStore: store, terminal: make(chan engine.TaskResult, 1)}
	taskEngine := engine.NewEngine(t.Context(), map[engine.TaskType]engine.TaskHandler{engine.TaskSnapshotRestore: restorer.Handler()}, observed)
	if _, err := taskEngine.Submit(engine.Task{ID: restoreRecoveryTaskID, Type: engine.TaskSnapshotRestore, Params: map[string]any{"targetHeight": 1500}}); err != nil {
		t.Fatal(err)
	}
	result := <-observed.terminal
	if result.Status != engine.TaskStatusCompleted || result.Error != "" {
		t.Fatalf("ordinary restore failed: %+v", result)
	}
	body, err := os.ReadFile(filepath.Join(home, "data", "snapshots", snapshotInstallationReceiptFile))
	if err != nil {
		t.Fatal(err)
	}
	var receipt restoreInstallationReceipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	wantIdentity := restoreInstallationIdentity{TaskID: restoreRecoveryTaskID, Bucket: "bucket", Region: "region", ChainID: "chain", TargetHeight: 1500}
	if receipt.Identity != wantIdentity || receipt.Height != 1000 {
		t.Fatalf("installed receipt does not identify the actual task and selected height: %+v", receipt)
	}
	requireDiagnosticSnapshotMembership(t, home, 1000)
	requireRestoreNeighbors(t, home)
}

func TestSnapshotRestoreEngineRecoveryRejectsUnrelatedStore(t *testing.T) {
	home := t.TempDir()
	writeRestoreNeighbors(t, home)
	previous := createRestoreDiagnosticArchive(t, 2000, 3)
	destination := filepath.Join(home, "data", "snapshots")
	if err := os.Rename(previous.directory, destination); err != nil {
		t.Fatal(err)
	}
	before := captureRestoreTree(t, home)
	store, err := engine.NewSQLiteStore(filepath.Join(t.TempDir(), "sidecar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	running := &engine.TaskResult{ID: restoreRecoveryTaskID, Type: string(engine.TaskSnapshotRestore), Status: engine.TaskStatusRunning, Run: 1, Params: map[string]any{"targetHeight": 1500}}
	if err := store.Save(running); err != nil {
		t.Fatal(err)
	}
	observed := &restoreRecoveryStore{ResultStore: store, terminal: make(chan engine.TaskResult, 1)}
	restorer := diagnosticRestorer(t, home, nil)
	restarted := engine.NewEngine(t.Context(), map[engine.TaskType]engine.TaskHandler{engine.TaskSnapshotRestore: restorer.Handler()}, observed)
	restarted.RehydrateStaleTasks()
	result := <-observed.terminal
	if result.Status != engine.TaskStatusFailed || !strings.Contains(result.Error, "is not empty") {
		t.Fatalf("unrelated store was not rejected: %+v", result)
	}
	if !reflect.DeepEqual(before, captureRestoreTree(t, home)) {
		t.Fatal("engine recovery changed the unrelated store or records")
	}
	requireDiagnosticSnapshotMembership(t, home, 2000)
	requireRestoreNeighbors(t, home)
}

func TestSnapshotRestoreRecoveryRequiresMatchingInstallation(t *testing.T) {
	for _, mismatch := range []string{"task", "bucket", "region", "chain", "target", "no task", "no receipt", "malformed receipt", "invalid height", "symlink receipt"} {
		t.Run(mismatch, func(t *testing.T) {
			home := t.TempDir()
			writeRestoreNeighbors(t, home)
			blocker := filepath.Join(home, SnapshotHeightFile)
			if err := os.Mkdir(blocker, 0o755); err != nil {
				t.Fatal(err)
			}
			incoming := createRestoreDiagnosticArchive(t, 1000, 0)
			restorer := diagnosticRestorer(t, home, map[string][]byte{"1000": diagnosticChunksFirstArchive(t, incoming.directory)})
			ctx := engine.WithTaskID(t.Context(), restoreRecoveryTaskID)
			if err := restorer.Restore(ctx, 1500); err == nil || !strings.Contains(err.Error(), "snapshot installed but height recording failed") {
				t.Fatalf("fixture did not install before record failure: %v", err)
			}
			if err := os.Remove(blocker); err != nil {
				t.Fatal(err)
			}
			receiptPath := filepath.Join(home, "data", "snapshots", snapshotInstallationReceiptFile)
			target := int64(1500)
			switch mismatch {
			case "task":
				ctx = engine.WithTaskID(t.Context(), "22222222-2222-4222-8222-222222222222")
			case "bucket":
				restorer.bucket = "other-bucket"
			case "region":
				restorer.region = "other-region"
			case "chain":
				restorer.chainID = "other-chain"
			case "target":
				target = 1600
			case "no task":
				ctx = t.Context()
			case "no receipt":
				if err := os.Remove(receiptPath); err != nil {
					t.Fatal(err)
				}
			case "malformed receipt":
				writeRestoreTestFile(t, receiptPath, "{")
			case "invalid height":
				receipt := restoreInstallationReceipt{Identity: restoreInstallationIdentity{TaskID: restoreRecoveryTaskID, Bucket: "bucket", Region: "region", ChainID: "chain", TargetHeight: 1500}, Height: 1600}
				body, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				writeRestoreTestFile(t, receiptPath, string(body))
			case "symlink receipt":
				if err := os.Remove(receiptPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(home, uploadStateFile), receiptPath); err != nil {
					t.Fatal(err)
				}
			}
			before := captureRestoreTree(t, home)
			if err := restorer.Restore(ctx, target); err == nil {
				t.Fatal("unowned or invalid installation was accepted")
			}
			if !reflect.DeepEqual(before, captureRestoreTree(t, home)) {
				t.Fatal("rejected recovery modified installed files or records")
			}
			requireRestoreNeighbors(t, home)
		})
	}
}

// Hidden archive entries are refused by extraction. A remote receipt must never
// authorize recovery; retrying with an ordinary archive establishes local ownership.
func TestSnapshotRestoreRejectsArchiveInstallationReceipt(t *testing.T) {
	home := t.TempDir()
	writeRestoreNeighbors(t, home)
	incoming := createRestoreDiagnosticArchive(t, 1000, 0)
	normal := diagnosticChunksFirstArchive(t, incoming.directory)
	writeRestoreTestFile(t, filepath.Join(incoming.directory, snapshotInstallationReceiptFile), `{"identity":{"taskID":"foreign-task"},"height":9999}`)
	restorer := diagnosticRestorer(t, home, map[string][]byte{"1000": diagnosticChunksFirstArchive(t, incoming.directory)})
	ctx := engine.WithTaskID(t.Context(), restoreRecoveryTaskID)
	before := captureRestoreTree(t, home)
	if err := restorer.Restore(ctx, 1500); err == nil {
		t.Fatal("archive installation receipt was accepted")
	}
	if !reflect.DeepEqual(before, captureRestoreTree(t, home)) {
		t.Fatal("rejected archive changed existing data or records")
	}
	if _, err := os.Lstat(filepath.Join(home, "data", "snapshots")); !os.IsNotExist(err) {
		t.Fatalf("rejected archive installed a destination: %v", err)
	}
	restorer.clientFactory = mockClientFactory(&mockTransferClient{responses: map[string][]byte{"chain/state-sync/1000.tar.gz": normal}})
	if err := restorer.Restore(ctx, 1500); err != nil {
		t.Fatalf("ordinary archive retry failed: %v", err)
	}
	requireDiagnosticSnapshotMembership(t, home, 1000)
	requireRestoreNeighbors(t, home)
	requireNoRestoreStaging(t, home)
}
