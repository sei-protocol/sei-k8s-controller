package hashlogcompare

import (
	"context"
	"fmt"
	"slices"
	"strings"

	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

const testHeader = "block_number,blockHash,changeset,resultHash,memIAVL/mod/bank,memIAVL/mod/evm\n"

// row renders one hash-log line; tweak changes the changeset hash.
func row(height int64, tweak string) string {
	return fmt.Sprintf("%d,bh%d,cs%d%s,rh%d,bank%d,evm%d\n", height, height, height, tweak, height, height, height)
}

func rows(from, to int64) string {
	var b strings.Builder
	for h := from; h <= to; h++ {
		b.WriteString(row(h, ""))
	}
	return b.String()
}

type fakeFile struct {
	index  uint64
	name   string
	sealed bool
	data   []byte
}

// fakeSource serves in-memory files with the sidecar's 404 and 416 semantics.
type fakeSource struct {
	files   []*fakeFile
	listErr error
	// served counts the bytes returned by reads.
	served int
}

func (f *fakeSource) ListHashLog(context.Context) ([]sidecar.HashLogFile, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]sidecar.HashLogFile, 0, len(f.files))
	for _, ff := range f.files {
		out = append(out, sidecar.HashLogFile{Name: ff.name, Index: ff.index, Size: int64(len(ff.data)), Sealed: ff.sealed})
	}
	slices.SortFunc(out, func(a, b sidecar.HashLogFile) int { return int(a.Index) - int(b.Index) })
	return out, nil
}

func (f *fakeSource) ReadHashLogFile(_ context.Context, name string, offset int64) ([]byte, error) {
	for _, ff := range f.files {
		if ff.name == name {
			if offset >= int64(len(ff.data)) {
				return nil, nil
			}
			f.served += len(ff.data) - int(offset)
			return slices.Clone(ff.data[offset:]), nil
		}
	}
	return nil, sidecar.ErrHashLogNotFound
}

func (f *fakeSource) ReadHashLogHead(_ context.Context, name string, length int64) ([]byte, error) {
	for _, ff := range f.files {
		if ff.name == name {
			n := min(int(length), len(ff.data))
			f.served += n
			return slices.Clone(ff.data[:n]), nil
		}
	}
	return nil, sidecar.ErrHashLogNotFound
}

func (f *fakeSource) add(index uint64, data string) *fakeFile {
	ff := &fakeFile{index: index, name: fmt.Sprintf("%d-v6.7.0.hlog.u", index), data: []byte(data)}
	f.files = append(f.files, ff)
	return ff
}

func (ff *fakeFile) seal(first, last int64, extra string) {
	ff.data = append(ff.data, extra...)
	ff.name = fmt.Sprintf("%d-%d-%d-v6.7.0.hlog", ff.index, first, last)
	ff.sealed = true
}

func (f *fakeSource) remove(index uint64) {
	f.files = slices.DeleteFunc(f.files, func(ff *fakeFile) bool { return ff.index == index })
}

func heights(rs []Row) []int64 {
	out := make([]int64, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Height)
	}
	return out
}

func span(from, to int64) []int64 {
	out := make([]int64, 0, to-from+1)
	for h := from; h <= to; h++ {
		out = append(out, h)
	}
	return out
}
