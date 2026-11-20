package chaoscheck

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

func init() {
	pollInterval = 10 * time.Millisecond
	sampleInterval = 5 * time.Millisecond
}

// node is a fake Tendermint RPC. /status reports its height; /abci_info reports
// the height and a hash of the node's own committed state, as the real endpoint
// does. hashAt decides the hash per height, so a test can make one node differ.
// enveloped selects the JSON-RPC wrapper the Sei fork sometimes drops, and
// quotedHeight selects the string or the bare-number height encoding.
type node struct {
	height       atomic.Int64
	hashAt       func(h int64) string
	enveloped    bool
	quotedHeight bool
	down         atomic.Bool
}

func (n *node) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		if n.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintf(w, `{"sync_info":{"latest_block_height":"%d","catching_up":false}}`, n.height.Load())
	})
	mux.HandleFunc("/abci_info", func(w http.ResponseWriter, _ *http.Request) {
		if n.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		h := n.height.Load()
		height := fmt.Sprintf("%d", h)
		if n.quotedHeight {
			height = fmt.Sprintf("%q", height)
		}
		body := fmt.Sprintf(`{"response":{"last_block_height":%s,"last_block_app_hash":%q}}`, height, n.hashAt(h))
		if n.enveloped {
			body = fmt.Sprintf(`{"result":%s}`, body)
		}
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// sameHash is the hash every honest node reports at a height.
func sameHash(h int64) string { return fmt.Sprintf("hash-%d", h) }

func newNode(t *testing.T, height int64, hashAt func(int64) string) (*node, string) {
	t.Helper()
	n := &node{hashAt: hashAt}
	n.height.Store(height)
	return n, n.serve(t).URL
}

// advance moves the node's height forward every few milliseconds until the test
// ends, so the chain keeps moving under WaitAgreeAbove.
func advance(t *testing.T, ns ...*node) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() { close(stop); <-done })
	go func() {
		defer close(done)
		tick := time.NewTicker(3 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				for _, n := range ns {
					n.height.Add(1)
				}
			}
		}
	}()
}

func within(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestInfo_AllResponseShapes(t *testing.T) {
	for _, enveloped := range []bool{true, false} {
		for _, quoted := range []bool{true, false} {
			t.Run(fmt.Sprintf("enveloped=%v quoted=%v", enveloped, quoted), func(t *testing.T) {
				g := NewWithT(t)
				n := &node{hashAt: sameHash, enveloped: enveloped, quotedHeight: quoted}
				n.height.Store(42)
				s, err := Info(context.Background(), nil, n.serve(t).URL)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(s).To(Equal(Sample{Height: 42, Hash: "hash-42"}))
			})
		}
	}
}

func TestInfo_NothingCommittedIsAnError(t *testing.T) {
	t.Run("height zero", func(t *testing.T) {
		g := NewWithT(t)
		_, url := newNode(t, 0, sameHash)
		_, err := Info(context.Background(), nil, url)
		g.Expect(err).To(MatchError(ContainSubstring("committed nothing")))
	})
	t.Run("empty hash would otherwise compare equal everywhere", func(t *testing.T) {
		g := NewWithT(t)
		_, url := newNode(t, 10, func(int64) string { return "" })
		_, err := Info(context.Background(), nil, url)
		g.Expect(err).To(MatchError(ContainSubstring("committed nothing")))
	})
}

func TestFloor(t *testing.T) {
	g := NewWithT(t)
	_, a := newNode(t, 100, sameHash)
	_, b := newNode(t, 90, sameHash)
	down, c := newNode(t, 500, sameHash)
	down.down.Store(true)

	f, err := Floor(context.Background(), nil, []string{a, b, c})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(f).To(Equal(int64(100)), "highest height among the reachable nodes; the unreachable one is skipped")

	_, err = Floor(context.Background(), nil, []string{c})
	g.Expect(err).To(HaveOccurred(), "fails only when no node answers")
}

func TestWaitAgreeAbove_AgreesOnceEveryNodeIsPastTheFloor(t *testing.T) {
	g := NewWithT(t)
	a, urlA := newNode(t, 100, sameHash)
	b, urlB := newNode(t, 100, sameHash)
	c, urlC := newNode(t, 100, sameHash)
	advance(t, a, b, c)
	g.Expect(WaitAgreeAbove(within(t, 5*time.Second), nil, []string{urlA, urlB, urlC}, 100, 3)).To(Succeed())
}

// A validator that restarted and is still replaying behind the fault must not
// let the check pass at a height from before the fault.
func TestWaitAgreeAbove_LaggingNodeBelowTheFloorFails(t *testing.T) {
	g := NewWithT(t)
	fast, urlFast := newNode(t, 1000, sameHash)
	_, urlLagging := newNode(t, 50, sameHash) // stays at 50: below the floor
	advance(t, fast)

	err := WaitAgreeAbove(within(t, 300*time.Millisecond), nil, []string{urlFast, urlLagging}, 900, 3)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(urlLagging), "the error must name the node that never got past the floor")
	var div *DivergenceError
	g.Expect(errors.As(err, &div)).To(BeFalse(), "being behind is not a divergence")
}

func TestWaitAgreeAbove_DivergenceReturnsAtOnce(t *testing.T) {
	g := NewWithT(t)
	a, urlA := newNode(t, 100, sameHash)
	b, urlB := newNode(t, 100, func(h int64) string { return fmt.Sprintf("other-%d", h) })
	advance(t, a, b)

	start := time.Now()
	err := WaitAgreeAbove(within(t, 5*time.Second), nil, []string{urlA, urlB}, 100, 3)
	var div *DivergenceError
	g.Expect(errors.As(err, &div)).To(BeTrue())
	g.Expect(div.Hashes).To(HaveLen(2))
	g.Expect(time.Since(start)).To(BeNumerically("<", time.Second), "a divergence must not be retried to the deadline")
}

func TestWaitAgreeAbove_NodeReportingTwoHashesForOneHeightDiverges(t *testing.T) {
	g := NewWithT(t)
	var flips atomic.Int64
	// Same height every read, a different hash on each read.
	n := &node{hashAt: func(int64) string { return fmt.Sprintf("hash-%d", flips.Add(1)) }}
	n.height.Store(500)
	url := n.serve(t).URL
	_, other := newNode(t, 500, sameHash)

	err := WaitAgreeAbove(within(t, 2*time.Second), nil, []string{url, other}, 100, 3)
	var div *DivergenceError
	g.Expect(errors.As(err, &div)).To(BeTrue())
}

func TestWaitAgreeAbove_UnreachableNodeNeverAgrees(t *testing.T) {
	g := NewWithT(t)
	a, urlA := newNode(t, 100, sameHash)
	b, urlB := newNode(t, 100, sameHash)
	b.down.Store(true)
	advance(t, a)

	err := WaitAgreeAbove(within(t, 300*time.Millisecond), nil, []string{urlA, urlB}, 50, 3)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring(urlB))
}

func TestWaitAgreeAbove_NoEndpoints(t *testing.T) {
	g := NewWithT(t)
	g.Expect(WaitAgreeAbove(context.Background(), nil, nil, 0, 1)).To(MatchError(ContainSubstring("no RPC endpoints")))
}

func TestHoldsHeight(t *testing.T) {
	t.Run("a stalled chain holds", func(t *testing.T) {
		g := NewWithT(t)
		_, url := newNode(t, 50, sameHash)
		g.Expect(HoldsHeight(context.Background(), nil, url, 0, 100*time.Millisecond, 1)).To(Succeed())
	})
	t.Run("a chain still committing fails", func(t *testing.T) {
		g := NewWithT(t)
		n, url := newNode(t, 50, sameHash)
		advance(t, n)
		err := HoldsHeight(context.Background(), nil, url, 0, 500*time.Millisecond, 1)
		g.Expect(err).To(MatchError(ContainSubstring("quorum should be lost")))
	})
	t.Run("one in-flight block is within tolerance", func(t *testing.T) {
		g := NewWithT(t)
		n, url := newNode(t, 50, sameHash)
		go func() { time.Sleep(20 * time.Millisecond); n.height.Add(1) }()
		g.Expect(HoldsHeight(context.Background(), nil, url, 0, 200*time.Millisecond, 1)).To(Succeed())
	})
}

func TestHoldsHeight_UnreachableNodeIsNotAHeldChain(t *testing.T) {
	g := NewWithT(t)
	// The node answers /status for the baseline read, then goes silent.
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) > 1 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"sync_info":{"latest_block_height":"50","catching_up":false}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	err := HoldsHeight(context.Background(), nil, srv.URL, 0, 500*time.Millisecond, 1)
	g.Expect(err).To(MatchError(ContainSubstring("no height")),
		"a node that went silent must not be reported as a chain that held")
}

// last_block_height is omitted at height 0, which arrives as no field at all.
func TestInfo_OmittedHeightReadsAsNothingCommitted(t *testing.T) {
	g := NewWithT(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/abci_info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"response":{"data":"sei"}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, err := Info(context.Background(), nil, srv.URL)
	g.Expect(err).To(MatchError(ContainSubstring("committed nothing")))
}
