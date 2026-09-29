package hashlogcompare

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"strconv"

	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

const (
	// tipBacktrackBytes is how far before the end of the newest file a fresh
	// reader starts, so the first poll yields some recent rows to align on.
	tipBacktrackBytes = 16 << 10
	// maxReadsPerPoll bounds the work one Poll does when a node has a backlog.
	maxReadsPerPoll = 16
	// maxHeaderBytes bounds the read a fresh reader makes for a file's header.
	maxHeaderBytes = 64 << 10
)

// errCoverageGap means rows were lost between two reads (a file was pruned
// while it was being read), so the reader restarts at the tip.
var errCoverageGap = errors.New("hash log coverage gap")

// Source is the subset of the sidecar client the reader uses.
type Source interface {
	ListHashLog(ctx context.Context) ([]sidecar.HashLogFile, error)
	ReadHashLogFile(ctx context.Context, name string, offset int64) ([]byte, error)
	ReadHashLogHead(ctx context.Context, name string, length int64) ([]byte, error)
}

// Row is one hash-log line: the block height and every hash column by header name.
type Row struct {
	Height int64
	Hashes map[string]string
}

// Reader tails one node's hash log in file-index order. It keeps a byte
// cursor into the current file and only consumes complete lines, so a line
// that is half written when read is picked up whole on the next poll.
type Reader struct {
	src Source

	started     bool
	index       uint64
	name        string
	sealed      bool
	offset      int64
	header      []string
	skipPartial bool
}

// NewReader returns a reader that starts near the tip of the newest file.
func NewReader(src Source) *Reader {
	return &Reader{src: src}
}

// Poll returns the complete rows written since the last call, plus the number
// of lines that could not be parsed. Transport errors leave the cursor where
// it was; errCoverageGap resets it to the tip.
func (r *Reader) Poll(ctx context.Context) (rows []Row, invalid int, err error) {
	if !r.started {
		ok, err := r.start(ctx)
		if err != nil || !ok {
			return nil, 0, err
		}
	}
	for range maxReadsPerPoll {
		data, err := r.src.ReadHashLogFile(ctx, r.name, r.offset)
		if errors.Is(err, sidecar.ErrHashLogNotFound) {
			more, err := r.handleMissing(ctx)
			if err != nil || !more {
				return rows, invalid, err
			}
			continue
		}
		if err != nil {
			return rows, invalid, err
		}

		if r.header == nil {
			nl := bytes.IndexByte(data, '\n')
			if nl < 0 {
				return rows, invalid, nil
			}
			header, err := parseHeader(data[:nl])
			if err != nil {
				return rows, invalid, err
			}
			r.header = header
			r.offset += int64(nl + 1)
			data = data[nl+1:]
		}
		if r.skipPartial {
			nl := bytes.IndexByte(data, '\n')
			if nl < 0 {
				r.offset += int64(len(data))
				return rows, invalid, nil
			}
			r.offset += int64(nl + 1)
			data = data[nl+1:]
			r.skipPartial = false
		}

		complete := data[:bytes.LastIndexByte(data, '\n')+1]
		r.offset += int64(len(complete))
		parsed, bad := r.parseRows(complete)
		rows = append(rows, parsed...)
		invalid += bad

		if !r.sealed || len(complete) < len(data) {
			return rows, invalid, nil
		}
		more, err := r.nextFile(ctx)
		if err != nil || !more {
			return rows, invalid, err
		}
	}
	return rows, invalid, nil
}

// start positions the cursor near the end of the newest file.
func (r *Reader) start(ctx context.Context) (bool, error) {
	files, err := r.src.ListHashLog(ctx)
	if err != nil || len(files) == 0 {
		return false, err
	}
	latest := files[len(files)-1]
	head, err := r.src.ReadHashLogHead(ctx, latest.Name, maxHeaderBytes)
	if errors.Is(err, sidecar.ErrHashLogNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	nl := bytes.IndexByte(head, '\n')
	if nl < 0 {
		if len(head) >= maxHeaderBytes {
			return false, fmt.Errorf("hash log %s header exceeds %d bytes", latest.Name, maxHeaderBytes)
		}
		return false, nil
	}
	header, err := parseHeader(head[:nl])
	if err != nil {
		return false, err
	}
	r.started = true
	r.index, r.name, r.sealed, r.header = latest.Index, latest.Name, latest.Sealed, header
	headerEnd := int64(nl + 1)
	r.offset = max(headerEnd, max(latest.Size, int64(len(head)))-tipBacktrackBytes)
	r.skipPartial = r.offset > headerEnd
	return true, nil
}

// handleMissing follows the current file after a 404. The HashLogger renames
// a file when it seals it, keeping the index, so the same index under a new
// name is the same bytes. A file gone entirely was pruned.
func (r *Reader) handleMissing(ctx context.Context) (bool, error) {
	files, err := r.src.ListHashLog(ctx)
	if err != nil {
		return false, err
	}
	for _, f := range files {
		if f.Index == r.index {
			if f.Name == r.name {
				return false, nil
			}
			r.name, r.sealed = f.Name, f.Sealed
			return true, nil
		}
	}
	if r.offset > 0 {
		r.started = false
		return false, fmt.Errorf("%w: file index %d was removed while it was read", errCoverageGap, r.index)
	}
	// The HashLogger removes a file that never received a row.
	return r.moveAfter(files), nil
}

func (r *Reader) nextFile(ctx context.Context) (bool, error) {
	files, err := r.src.ListHashLog(ctx)
	if err != nil {
		return false, err
	}
	return r.moveAfter(files), nil
}

func (r *Reader) moveAfter(files []sidecar.HashLogFile) bool {
	for _, f := range files {
		if f.Index > r.index {
			r.index, r.name, r.sealed = f.Index, f.Name, f.Sealed
			r.offset, r.header, r.skipPartial = 0, nil, false
			return true
		}
	}
	return false
}

func parseHeader(line []byte) ([]string, error) {
	fields, err := csv.NewReader(bytes.NewReader(line)).Read()
	if err != nil {
		return nil, fmt.Errorf("parse hash log header: %w", err)
	}
	if len(fields) < 2 || fields[0] != "block_number" {
		return nil, fmt.Errorf("unexpected hash log header %q", line)
	}
	return fields, nil
}

func (r *Reader) parseRows(data []byte) (rows []Row, invalid int) {
	for line := range bytes.Lines(data) {
		fields, err := csv.NewReader(bytes.NewReader(line)).Read()
		if err != nil || len(fields) != len(r.header) {
			invalid++
			continue
		}
		height, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			invalid++
			continue
		}
		hashes := make(map[string]string, len(fields)-1)
		for i := 1; i < len(fields); i++ {
			hashes[r.header[i]] = fields[i]
		}
		rows = append(rows, Row{Height: height, Hashes: hashes})
	}
	return rows, invalid
}
