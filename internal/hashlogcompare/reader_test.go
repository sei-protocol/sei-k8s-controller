package hashlogcompare

import (
	"fmt"
	"testing"

	. "github.com/onsi/gomega"

	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

func TestReader_StartsNearTipOnCompleteRow(t *testing.T) {
	g := NewWithT(t)
	src := &fakeSource{}
	src.add(1, testHeader+rows(1, 1000))
	r := NewReader(src)

	got, invalid, err := r.Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(invalid).To(BeZero())
	g.Expect(got).NotTo(BeEmpty())
	g.Expect(got[0].Height).To(BeNumerically(">", 1))
	g.Expect(heights(got)).To(Equal(span(got[0].Height, 1000)))
	g.Expect(got[0].Hashes).To(HaveKeyWithValue("changeset", fmt.Sprintf("cs%d", got[0].Height)))
}

func TestReader_StartReadIsBounded(t *testing.T) {
	g := NewWithT(t)
	src := &fakeSource{}
	src.add(1, testHeader+rows(1, 100_000))

	got, _, err := NewReader(src).Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got[len(got)-1].Height).To(Equal(int64(100_000)))
	g.Expect(src.served).To(BeNumerically("<=", maxHeaderBytes+tipBacktrackBytes))
}

func TestReader_SmallFileReadsFromFirstRow(t *testing.T) {
	g := NewWithT(t)
	src := &fakeSource{}
	src.add(1, testHeader+rows(1, 5))

	got, _, err := NewReader(src).Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(heights(got)).To(Equal(span(1, 5)))
}

func TestReader_WaitsForPartialLine(t *testing.T) {
	g := NewWithT(t)
	src := &fakeSource{}
	line := row(3, "")
	f := src.add(1, testHeader+rows(1, 2)+line[:5])
	r := NewReader(src)

	got, _, err := r.Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(heights(got)).To(Equal(span(1, 2)))

	f.data = append(f.data, line[5:]...)
	got, _, err = r.Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(heights(got)).To(Equal([]int64{3}))
}

func TestReader_FollowsSealAndNextFile(t *testing.T) {
	g := NewWithT(t)
	src := &fakeSource{}
	f := src.add(1, testHeader+rows(1, 3))
	r := NewReader(src)

	got, _, err := r.Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(heights(got)).To(Equal(span(1, 3)))

	f.seal(1, 5, rows(4, 5))
	src.add(2, testHeader+rows(6, 7))
	got, _, err = r.Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(heights(got)).To(Equal(span(4, 7)))

	got, _, err = r.Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(BeEmpty())
}

func TestReader_DropsTornTailOfSealedFile(t *testing.T) {
	g := NewWithT(t)
	src := &fakeSource{}
	f := src.add(1, testHeader+rows(10, 11))
	r := NewReader(src)

	got, _, err := r.Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(heights(got)).To(Equal(span(10, 11)))

	f.seal(10, 12, row(12, "")+row(13, "")[:5])
	src.add(2, testHeader+rows(13, 14))
	got, _, err = r.Poll(t.Context())
	g.Expect(err).To(MatchError(errTornRow))
	g.Expect(heights(got)).To(Equal([]int64{12}))

	got, _, err = r.Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(heights(got)).To(Equal(span(13, 14)))
}

func TestReader_SkipsRemovedEmptyFile(t *testing.T) {
	g := NewWithT(t)
	src := &fakeSource{}
	f := src.add(1, testHeader+rows(1, 2))
	r := NewReader(src)
	_, _, err := r.Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())

	f.seal(1, 2, "")
	src.add(2, "")
	got, _, err := r.Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(BeEmpty())

	src.remove(2)
	src.add(3, testHeader+rows(3, 4))
	got, _, err = r.Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(heights(got)).To(Equal(span(3, 4)))
}

func TestReader_PrunedWhileReadingIsCoverageGap(t *testing.T) {
	g := NewWithT(t)
	src := &fakeSource{}
	src.add(1, testHeader+rows(1, 2))
	r := NewReader(src)
	_, _, err := r.Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())

	src.remove(1)
	src.add(2, testHeader+rows(10, 11))
	_, _, err = r.Poll(t.Context())
	g.Expect(err).To(MatchError(errCoverageGap))

	got, _, err := r.Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(heights(got)).To(Equal(span(10, 11)))
}

func TestReader_CountsInvalidLines(t *testing.T) {
	g := NewWithT(t)
	src := &fakeSource{}
	src.add(1, testHeader+row(1, "")+"2,short\n"+"x,a,b,c,d,e\n"+row(4, ""))

	got, invalid, err := NewReader(src).Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(invalid).To(Equal(2))
	g.Expect(heights(got)).To(Equal([]int64{1, 4}))
}

func TestReader_MissingDirectoryIsNotFound(t *testing.T) {
	g := NewWithT(t)
	src := &fakeSource{listErr: sidecar.ErrHashLogNotFound}

	_, _, err := NewReader(src).Poll(t.Context())
	g.Expect(err).To(MatchError(sidecar.ErrHashLogNotFound))
}

func TestReader_EmptyDirectoryYieldsNothing(t *testing.T) {
	g := NewWithT(t)
	got, _, err := NewReader(&fakeSource{}).Poll(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(BeEmpty())
}
