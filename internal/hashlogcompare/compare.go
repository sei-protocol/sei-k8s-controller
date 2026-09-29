package hashlogcompare

import (
	"errors"
	"maps"
	"slices"
	"strings"
)

// maxBufferedRows bounds each side's unmatched rows while the other side is
// behind or down, about an hour of blocks. The staleness alert covers that
// case; this only caps memory. Overflow evicts evictBatchRows at a time.
const (
	maxBufferedRows = 10_000
	evictBatchRows  = 1_000
)

// Hash-log column names every comparable row carries.
const (
	ColumnBlockHash  = "blockHash"
	ColumnChangeset  = "changeset"
	ColumnResultHash = "resultHash"

	memIAVLModulePrefix = "memIAVL/mod/"
	memIAVLEVMColumn    = memIAVLModulePrefix + "evm"
)

var (
	baseComparable = []string{ColumnBlockHash, ColumnChangeset, ColumnResultHash}

	errMissingBase    = errors.New("row is missing blockHash, changeset or resultHash")
	errNoModuleHashes = errors.New("row has no comparable non-EVM module hashes")
)

// ComparableHashes returns the columns that must match between a FlatKV
// migrating node and a memIAVL-only reserve: the block, changeset and result
// hashes, and every memIAVL module root except EVM, whose storage the
// migration moves.
func ComparableHashes(hashes map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(hashes))
	for _, name := range baseComparable {
		v, ok := hashes[name]
		if !ok {
			return nil, errMissingBase
		}
		out[name] = v
	}
	for name, v := range hashes {
		if strings.HasPrefix(name, memIAVLModulePrefix) && name != memIAVLEVMColumn {
			out[name] = v
		}
	}
	if len(out) == len(baseComparable) {
		return nil, errNoModuleHashes
	}
	return out, nil
}

// Mismatch is one compared height where the two nodes disagree.
type Mismatch struct {
	Height  int64
	Columns []string
}

// CompareResult is what one Compare call found.
type CompareResult struct {
	Compared   int
	Mismatches []Mismatch
	// MigratingGaps and ReserveGaps count heights that side passed without
	// writing a row the other side has, after the first compared height.
	MigratingGaps int
	ReserveGaps   int
}

// pairState buffers rows from both sides of a pair until both have a height.
type pairState struct {
	migrating, reserve map[int64]map[string]string

	firstCompared int64
	lastCompared  int64
}

func newPairState() *pairState {
	return &pairState{
		migrating: map[int64]map[string]string{},
		reserve:   map[int64]map[string]string{},
	}
}

func (p *pairState) add(buf map[int64]map[string]string, height int64, hashes map[string]string) {
	if p.lastCompared > 0 && height <= p.lastCompared {
		return
	}
	buf[height] = hashes
	if len(buf) > maxBufferedRows {
		evictOldest(buf, len(buf)-maxBufferedRows+evictBatchRows)
	}
}

// evictOldest drops the n lowest heights from buf.
func evictOldest(buf map[int64]map[string]string, n int) {
	heights := slices.Sorted(maps.Keys(buf))
	for _, h := range heights[:min(n, len(heights))] {
		delete(buf, h)
	}
}

// compare checks every height both sides have, in order, then drops rows the
// other side can no longer supply.
func (p *pairState) compare() CompareResult {
	var res CompareResult
	var heights []int64
	for h := range p.migrating {
		if _, ok := p.reserve[h]; ok {
			heights = append(heights, h)
		}
	}
	slices.Sort(heights)
	for _, h := range heights {
		if cols := differingColumns(p.migrating[h], p.reserve[h]); len(cols) > 0 {
			res.Mismatches = append(res.Mismatches, Mismatch{Height: h, Columns: cols})
		}
		delete(p.migrating, h)
		delete(p.reserve, h)
		if p.firstCompared == 0 {
			p.firstCompared = h
		}
		p.lastCompared = h
		res.Compared++
	}
	res.ReserveGaps = p.prune(p.migrating)
	res.MigratingGaps = p.prune(p.reserve)
	return res
}

// prune drops buf's rows at or below the last compared height and returns how
// many of them the other side skipped after comparison started.
func (p *pairState) prune(buf map[int64]map[string]string) int {
	gaps := 0
	for h := range buf {
		if h > p.lastCompared {
			continue
		}
		if h > p.firstCompared {
			gaps++
		}
		delete(buf, h)
	}
	return gaps
}

func differingColumns(a, b map[string]string) []string {
	var cols []string
	for name, v := range a {
		if w, ok := b[name]; !ok || w != v {
			cols = append(cols, name)
		}
	}
	for name := range b {
		if _, ok := a[name]; !ok {
			cols = append(cols, name)
		}
	}
	slices.Sort(cols)
	return cols
}
