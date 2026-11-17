package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const (
	snapshotStagingPrefix           = ".sei-snapshot-restore-"
	snapshotInstallationReceiptFile = ".sei-sidecar-restore-installation.json"
)

type restoreInstallationIdentity struct {
	TaskID       string `json:"taskID"`
	Bucket       string `json:"bucket"`
	Region       string `json:"region"`
	ChainID      string `json:"chainID"`
	TargetHeight int64  `json:"targetHeight"`
}

type restoreInstallationReceipt struct {
	Identity restoreInstallationIdentity `json:"identity"`
	Height   int64                       `json:"height"`
}

func matchingSnapshotInstallation(destination string, identity restoreInstallationIdentity) (int64, error) {
	if identity.TaskID == "" {
		return 0, nil
	}
	info, err := os.Lstat(destination)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("inspecting installed snapshot: %w", err)
	}
	if !info.IsDir() {
		return 0, nil
	}
	path := filepath.Join(destination, snapshotInstallationReceiptFile)
	info, err = os.Lstat(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("inspecting snapshot installation receipt: %w", err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("snapshot installation receipt must be a regular file; existing data was preserved")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("reading snapshot installation receipt: %w", err)
	}
	var receipt restoreInstallationReceipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		return 0, fmt.Errorf("decoding snapshot installation receipt: %w", err)
	}
	if receipt.Identity != identity {
		return 0, fmt.Errorf("snapshot installation belongs to a different restore task or request; existing data was preserved")
	}
	if receipt.Height <= 0 || (identity.TargetHeight > 0 && receipt.Height > identity.TargetHeight) {
		return 0, fmt.Errorf("snapshot installation receipt has an invalid height; existing data was preserved")
	}
	return receipt.Height, nil
}

// snapshotInstallation owns private extraction and installation into an empty destination.
type snapshotInstallation struct {
	destination string
	staging     string
}

func prepareSnapshotInstallation(destination string) (*snapshotInstallation, error) {
	if err := requireEmptySnapshotDestination(destination); err != nil {
		return nil, err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("creating snapshot parent: %w", err)
	}
	staging, err := os.MkdirTemp(parent, snapshotStagingPrefix)
	if err != nil {
		return nil, fmt.Errorf("creating snapshot staging directory: %w", err)
	}
	return &snapshotInstallation{destination: destination, staging: staging}, nil
}

func requireEmptySnapshotDestination(destination string) error {
	info, err := os.Lstat(destination)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting snapshot destination: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("snapshot destination %s must be an absent or empty directory; existing data was preserved", destination)
	}
	directory, err := os.Open(destination)
	if err != nil {
		return fmt.Errorf("opening snapshot destination: %w", err)
	}
	defer func() { _ = directory.Close() }()
	_, err = directory.Readdirnames(1)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading snapshot destination: %w", err)
	}
	return fmt.Errorf("snapshot destination %s is not empty; existing data was preserved, use an empty destination to retry", destination)
}

func (i *snapshotInstallation) install(ctx context.Context, archivePath string, receipt restoreInstallationReceipt) error {
	restoreLog.Info("extracting archive into staging", "dest", i.staging)
	if err := extractArchive(ctx, archivePath, i.staging); err != nil {
		return fmt.Errorf("extracting snapshot: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Write ownership locally after extraction so it becomes visible with the
	// installed store, before completion can be recorded.
	body, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("encoding snapshot installation receipt: %w", err)
	}
	if err := writeRestoreFile(i.staging, snapshotInstallationReceiptFile, body); err != nil {
		return fmt.Errorf("writing snapshot installation receipt: %w", err)
	}
	// Another attempt may have installed while this archive was downloading.
	if err := requireEmptySnapshotDestination(i.destination); err != nil {
		return err
	}
	if err := os.Chmod(i.staging, 0o755); err != nil {
		return fmt.Errorf("setting installed snapshot permissions: %w", err)
	}
	// Rmdir removes only an empty directory, never a file, symlink or populated
	// store that appeared after the check. Some filesystems cannot rename over
	// even an empty directory.
	if err := syscall.Rmdir(i.destination); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing empty snapshot destination: %w", err)
	}
	if err := os.Rename(i.staging, i.destination); err != nil {
		return fmt.Errorf("installing snapshot directory: %w", err)
	}
	return nil
}

func (i *snapshotInstallation) cleanup() {
	// Interrupted attempts can leave staging directories. Never reuse or remove
	// another attempt's directory: it may still belong to a running restore.
	if err := os.RemoveAll(i.staging); err != nil {
		restoreLog.Warn("failed to remove snapshot staging directory", "path", i.staging, "err", err)
	}
}

// writeRestoreFile publishes a complete restore record by renaming a closed file.
func writeRestoreFile(homeDir, name string, contents []byte) error {
	file, err := os.CreateTemp(homeDir, ".snapshot-record-*")
	if err != nil {
		return err
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()
	if err := file.Chmod(0o644); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(homeDir, name))
}

func snapshotRestoreCompleted(homeDir string) (bool, error) {
	path := filepath.Join(homeDir, restoreMarkerFile)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspecting snapshot completion record: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("snapshot completion record %s must be a regular file; existing data was preserved", path)
	}
	return true, nil
}
