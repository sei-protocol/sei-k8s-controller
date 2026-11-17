package tasks

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gogo/protobuf/proto"
	snapshottypes "github.com/sei-protocol/sei-chain/sei-cosmos/snapshots/types"
	db "github.com/tendermint/tm-db"
)

// writeSnapshotMetadata creates a closed database containing one snapshot record.
// The caller must supply an empty scratch directory.
func writeSnapshotMetadata(dir string, snapshot snapshottypes.Snapshot) (retErr error) {
	metadata, err := db.NewGoLevelDB("metadata", dir)
	if err != nil {
		return fmt.Errorf("creating archive metadata database: %w", err)
	}
	defer func() {
		if err := metadata.Close(); retErr == nil && err != nil {
			retErr = fmt.Errorf("closing archive metadata database: %w", err)
		}
	}()
	value, err := proto.Marshal(&snapshot)
	if err != nil {
		return fmt.Errorf("encoding archive snapshot metadata: %w", err)
	}
	// SDK snapshots.Store encodes keys as prefix 0x01, height and format in
	// big-endian order. It has no write API for metadata alone. Keep this persisted
	// contract here and verify archives by reopening them through that store.
	key := make([]byte, 13)
	key[0] = 0x01
	binary.BigEndian.PutUint64(key[1:9], snapshot.Height)
	binary.BigEndian.PutUint32(key[9:13], snapshot.Format)
	if err := metadata.SetSync(key, value); err != nil {
		return fmt.Errorf("writing archive snapshot metadata: %w", err)
	}
	return nil
}

// writeArchive streams exactly the chunks and metadata of the selected snapshot.
// metadataDir must contain a closed database prepared for this snapshot.
func writeArchive(ctx context.Context, pipe *io.PipeWriter, snapshotsDir, metadataDir string, snapshot snapshottypes.Snapshot) (retErr error) {
	defer func() { _ = pipe.CloseWithError(retErr) }()
	gzipWriter := gzip.NewWriter(pipe)
	tarWriter := tar.NewWriter(gzipWriter)
	snapshotHash := sha256.New()
	base := filepath.Join(strconv.FormatUint(snapshot.Height, 10), strconv.FormatUint(uint64(snapshot.Format), 10))
	for index := uint32(0); index < snapshot.Chunks; index++ {
		name := filepath.Join(base, strconv.FormatUint(uint64(index), 10))
		if err := archiveSnapshotChunk(ctx, tarWriter, snapshotsDir, name, snapshot.Metadata.ChunkHashes[index], snapshotHash); err != nil {
			return err
		}
	}
	if !bytes.Equal(snapshotHash.Sum(nil), snapshot.Hash) {
		return fmt.Errorf("snapshot %d does not match the producer hash", snapshot.Height)
	}
	if err := archiveMetadata(ctx, tarWriter, metadataDir); err != nil {
		return err
	}
	if err := tarWriter.Close(); err != nil {
		return fmt.Errorf("closing tar writer: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return fmt.Errorf("closing gzip writer: %w", err)
	}
	return nil
}

// archiveSnapshotChunk writes one expected chunk and verifies its hash while
// contributing its bytes to the aggregate snapshot hash.
func archiveSnapshotChunk(ctx context.Context, writer *tar.Writer, dir, name string, expectedHash []byte, snapshotHash hash.Hash) error {
	chunkHash := sha256.New()
	if err := archiveRegularFile(ctx, writer, filepath.Join(dir, name), name, chunkHash, snapshotHash); err != nil {
		return err
	}
	if !bytes.Equal(chunkHash.Sum(nil), expectedHash) {
		return fmt.Errorf("snapshot chunk %s does not match the producer hash", name)
	}
	return nil
}

// archiveMetadata writes the closed database prepared for this archive.
func archiveMetadata(ctx context.Context, writer *tar.Writer, dir string) error {
	metadataDir := filepath.Join(dir, "metadata.db")
	return filepath.WalkDir(metadataDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("reading archive metadata: %w", walkErr)
		}
		if entry.IsDir() {
			return nil
		}
		name, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		return archiveRegularFile(ctx, writer, path, name)
	})
}

// archiveRegularFile writes one regular file and feeds its bytes to the supplied
// hashers. Missing files and size changes abort the archive.
func archiveRegularFile(ctx context.Context, writer *tar.Writer, path, name string, hashers ...io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("reading archive file %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("archive file %s is not a regular file", name)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening archive file %s: %w", name, err)
	}
	defer func() { _ = file.Close() }()
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = filepath.ToSlash(name)
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	destinations := append([]io.Writer{writer}, hashers...)
	written, err := io.Copy(io.MultiWriter(destinations...), file)
	if err != nil {
		return fmt.Errorf("archiving file %s: %w", name, err)
	}
	if written != info.Size() {
		return fmt.Errorf("archive file %s changed size during upload", name)
	}
	return ctx.Err()
}
