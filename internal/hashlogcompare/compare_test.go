package hashlogcompare

import (
	"testing"

	. "github.com/onsi/gomega"
)

const bankColumn = memIAVLModulePrefix + "bank"

func hashes(changeset, evm string) map[string]string {
	return map[string]string{
		ColumnBlockHash: "bh", ColumnChangeset: changeset, ColumnResultHash: "rh",
		bankColumn: "bank", memIAVLEVMColumn: evm, "flatKV/evm": evm,
	}
}

func TestComparableHashes_DropsBackendSpecificColumns(t *testing.T) {
	g := NewWithT(t)
	got, err := ComparableHashes(hashes("cs", "evm"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(Equal(map[string]string{
		ColumnBlockHash: "bh", ColumnChangeset: "cs", ColumnResultHash: "rh", bankColumn: "bank",
	}))
}

func TestComparableHashes_RejectsIncompleteRows(t *testing.T) {
	g := NewWithT(t)
	_, err := ComparableHashes(map[string]string{ColumnBlockHash: "bh", ColumnChangeset: "cs", bankColumn: "b"})
	g.Expect(err).To(MatchError(errMissingBase))

	_, err = ComparableHashes(map[string]string{ColumnBlockHash: "bh", ColumnChangeset: "cs", ColumnResultHash: "rh"})
	g.Expect(err).To(MatchError(errNoModuleHashes))
}

func TestPairState_ComparesOnlyCommonHeightsInOrder(t *testing.T) {
	g := NewWithT(t)
	p := newPairState()
	for h := int64(1); h <= 5; h++ {
		p.add(p.migrating, h, map[string]string{ColumnChangeset: "cs"})
	}
	for h := int64(3); h <= 4; h++ {
		p.add(p.reserve, h, map[string]string{ColumnChangeset: "cs"})
	}

	res := p.compare()
	g.Expect(res.Compared).To(Equal(2))
	g.Expect(res.Mismatches).To(BeEmpty())
	g.Expect(res.MigratingGaps + res.ReserveGaps).To(BeZero())
	g.Expect(p.lastCompared).To(Equal(int64(4)))
	g.Expect(p.migrating).To(HaveLen(1))
	g.Expect(p.migrating).To(HaveKey(int64(5)))
}

func TestPairState_ReportsMismatchedColumns(t *testing.T) {
	g := NewWithT(t)
	p := newPairState()
	p.add(p.migrating, 7, map[string]string{ColumnChangeset: "a", ColumnResultHash: "r", bankColumn: "b"})
	p.add(p.reserve, 7, map[string]string{ColumnChangeset: "b", ColumnResultHash: "r"})

	res := p.compare()
	g.Expect(res.Mismatches).To(Equal([]Mismatch{{Height: 7, Columns: []string{ColumnChangeset, bankColumn}}}))
}

func TestPairState_CountsSkippedHeightsAfterComparisonStarts(t *testing.T) {
	g := NewWithT(t)
	p := newPairState()
	p.add(p.migrating, 10, map[string]string{})
	p.add(p.reserve, 10, map[string]string{})
	g.Expect(p.compare().Compared).To(Equal(1))

	p.add(p.migrating, 11, map[string]string{})
	p.add(p.migrating, 12, map[string]string{})
	p.add(p.reserve, 12, map[string]string{})
	res := p.compare()
	g.Expect(res.Compared).To(Equal(1))
	g.Expect(res.ReserveGaps).To(Equal(1))
	g.Expect(res.MigratingGaps).To(BeZero())

	p.add(p.reserve, 12, map[string]string{ColumnChangeset: "late"})
	g.Expect(p.reserve).To(BeEmpty())
}

func TestPairState_UnalignedStartIsNotAGap(t *testing.T) {
	g := NewWithT(t)
	p := newPairState()
	for h := int64(1); h <= 5; h++ {
		p.add(p.migrating, h, map[string]string{})
	}
	p.add(p.reserve, 5, map[string]string{})

	res := p.compare()
	g.Expect(res.Compared).To(Equal(1))
	g.Expect(res.MigratingGaps + res.ReserveGaps).To(BeZero())
	g.Expect(p.migrating).To(BeEmpty())
}
