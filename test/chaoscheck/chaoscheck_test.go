package chaoscheck

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

func init() { pollInterval = 10 * time.Millisecond }

// node is a fake Tendermint RPC: /status reports a height, /commit reports an
// app hash per height. enveloped selects the JSON-RPC wrapper the Sei fork
// sometimes drops.
type node struct {
	height    atomic.Int64
	hashes    map[int64]string
	enveloped bool
	noCommit  bool
}

func (n *node) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		body := fmt.Sprintf(`{"sync_info":{"latest_block_height":"%d","catching_up":false}}`, n.height.Load())
		if n.enveloped {
			body = fmt.Sprintf(`{"result":%s}`, body)
		}
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/commit", func(w http.ResponseWriter, r *http.Request) {
		if n.noCommit {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		h, _ := strconv.ParseInt(r.URL.Query().Get("height"), 10, 64)
		hdr := fmt.Sprintf(`{"header":{"app_hash":%q}}`, n.hashes[h])
		body := fmt.Sprintf(`{"signed_header":%s}`, hdr)
		if n.enveloped {
			body = fmt.Sprintf(`{"result":%s}`, body)
		}
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newNode(t *testing.T, height int64, hash string, enveloped bool) (*node, string) {
	t.Helper()
	n := &node{hashes: map[int64]string{}, enveloped: enveloped}
	n.height.Store(height)
	for h := int64(1); h <= height; h++ {
		n.hashes[h] = hash
	}
	return n, n.serve(t).URL
}

func TestAppHash_BothResponseShapes(t *testing.T) {
	for _, enveloped := range []bool{true, false} {
		t.Run(fmt.Sprintf("enveloped=%v", enveloped), func(t *testing.T) {
			g := NewWithT(t)
			_, url := newNode(t, 10, "abcd", enveloped)
			h, err := AppHash(context.Background(), nil, url, 5)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(h).To(Equal("ABCD"), "hashes are compared upper-cased")
		})
	}
}

func TestAppHash_EmptyHashIsAnError(t *testing.T) {
	g := NewWithT(t)
	n, url := newNode(t, 10, "abcd", false)
	n.hashes[5] = ""
	_, err := AppHash(context.Background(), nil, url, 5)
	g.Expect(err).To(MatchError(ContainSubstring("no app_hash")))
}

func TestAgreeOn(t *testing.T) {
	t.Run("equal hashes agree, whatever the case", func(t *testing.T) {
		g := NewWithT(t)
		_, a := newNode(t, 10, "abcd", false)
		_, b := newNode(t, 10, "ABCD", true)
		g.Expect(AgreeOn(context.Background(), nil, []string{a, b}, 5)).To(Succeed())
	})
	t.Run("different hashes diverge and name every node", func(t *testing.T) {
		g := NewWithT(t)
		_, a := newNode(t, 10, "aaaa", false)
		_, b := newNode(t, 10, "bbbb", false)
		err := AgreeOn(context.Background(), nil, []string{a, b}, 5)
		var div *DivergenceError
		g.Expect(errors.As(err, &div)).To(BeTrue())
		g.Expect(div.Height).To(Equal(int64(5)))
		g.Expect(div.Hashes).To(HaveLen(2))
	})
	t.Run("a read failure is not reported as divergence", func(t *testing.T) {
		g := NewWithT(t)
		_, a := newNode(t, 10, "aaaa", false)
		n, b := newNode(t, 10, "aaaa", false)
		n.noCommit = true
		err := AgreeOn(context.Background(), nil, []string{a, b}, 5)
		var div *DivergenceError
		g.Expect(err).To(HaveOccurred())
		g.Expect(errors.As(err, &div)).To(BeFalse())
	})
}

func TestCommonHeight(t *testing.T) {
	g := NewWithT(t)
	_, a := newNode(t, 100, "x", false)
	_, b := newNode(t, 90, "x", true)
	h, err := CommonHeight(context.Background(), nil, []string{a, b}, 5)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(h).To(Equal(int64(85)), "slowest node's height minus the margin")

	_, err = CommonHeight(context.Background(), nil, []string{a, b}, 95)
	g.Expect(err).To(MatchError(ContainSubstring("too low")))

	_, err = CommonHeight(context.Background(), nil, nil, 1)
	g.Expect(err).To(HaveOccurred())
}

func TestWaitAgree_RetriesUntilTheSlowNodeCatchesUp(t *testing.T) {
	g := NewWithT(t)
	_, fast := newNode(t, 100, "same", false)
	// The slow node has every hash from the start but reports a low height;
	// margin 5 makes that too low to compare until it advances. Only the height
	// changes after the server starts, so the hash map is never written while
	// the handler reads it.
	slow, slowURL := newNode(t, 60, "same", false)
	slow.height.Store(3)
	go func() {
		time.Sleep(50 * time.Millisecond)
		slow.height.Store(60)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g.Expect(WaitAgree(ctx, nil, []string{fast, slowURL}, 5)).To(Succeed())
}

func TestWaitAgree_DivergenceReturnsAtOnce(t *testing.T) {
	g := NewWithT(t)
	_, a := newNode(t, 100, "aaaa", false)
	_, b := newNode(t, 100, "bbbb", false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := WaitAgree(ctx, nil, []string{a, b}, 5)
	var div *DivergenceError
	g.Expect(errors.As(err, &div)).To(BeTrue())
	g.Expect(time.Since(start)).To(BeNumerically("<", time.Second), "a divergence must not be retried to the deadline")
}

func TestHoldsHeight(t *testing.T) {
	t.Run("a stalled chain holds", func(t *testing.T) {
		g := NewWithT(t)
		_, url := newNode(t, 50, "x", false)
		g.Expect(HoldsHeight(context.Background(), nil, url, 0, 100*time.Millisecond, 1)).To(Succeed())
	})
	t.Run("a chain still committing fails", func(t *testing.T) {
		g := NewWithT(t)
		n, url := newNode(t, 50, "x", false)
		go func() {
			for range 100 {
				n.height.Add(1)
				time.Sleep(5 * time.Millisecond)
			}
		}()
		err := HoldsHeight(context.Background(), nil, url, 0, 500*time.Millisecond, 1)
		g.Expect(err).To(MatchError(ContainSubstring("quorum should be lost")))
	})
	t.Run("one in-flight block is within tolerance", func(t *testing.T) {
		g := NewWithT(t)
		n, url := newNode(t, 50, "x", false)
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
