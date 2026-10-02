package evmdigestcompare

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

// fakeSidecar serves just the endpoints SidecarSource uses, with scripted
// task outcomes keyed on submission order.
type fakeSidecar struct {
	tip      *int64
	outcomes []fakeOutcome
	submits  []sidecar.TaskRequest
	deleted  []string
}

type fakeOutcome struct {
	status sidecar.TaskResultStatus
	result json.RawMessage
	err    string
}

func (f *fakeSidecar) handler() http.Handler {
	mux := http.NewServeMux()
	jsonBody := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
	}
	mux.HandleFunc("GET /v0/status", func(w http.ResponseWriter, _ *http.Request) {
		jsonBody(w)
		var h any
		if f.tip != nil {
			h = *f.tip
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "Ready", "committedHeight": h})
	})
	mux.HandleFunc("POST /v0/tasks", func(w http.ResponseWriter, r *http.Request) {
		var req sidecar.TaskRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.submits = append(f.submits, req)
		id := "00000000-0000-0000-0000-00000000000" + string(rune('0'+len(f.submits)))
		jsonBody(w)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	})
	mux.HandleFunc("GET /v0/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		i := 0
		if tail := strings.TrimPrefix(r.PathValue("id"), "00000000-0000-0000-0000-00000000000"); len(tail) > 0 {
			i = int(tail[0] - '1')
		}
		if i < 0 || i >= len(f.outcomes) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		oc := f.outcomes[i]
		tr := map[string]any{"id": r.PathValue("id"), "type": "evm-digest", "status": string(oc.status)}
		if oc.result != nil {
			tr["result"] = oc.result
		}
		if oc.err != "" {
			tr["error"] = oc.err
		}
		jsonBody(w)
		_ = json.NewEncoder(w).Encode(tr)
	})
	mux.HandleFunc("DELETE /v0/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.deleted = append(f.deleted, r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func newTestSource(t *testing.T, f *fakeSidecar) *SidecarSource {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client, err := sidecar.NewSidecarClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &SidecarSource{
		Client:      client,
		TaskPoll:    time.Millisecond,
		ScanTimeout: 5 * time.Second,
		Attempts:    1,
	}
}

func TestSidecarSource_Tip(t *testing.T) {
	g := NewWithT(t)
	tip := int64(123)
	src := newTestSource(t, &fakeSidecar{tip: &tip})
	got, err := src.Tip(t.Context())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got).To(Equal(int64(123)))
}

func TestSidecarSource_TipMissingHeight(t *testing.T) {
	g := NewWithT(t)
	src := newTestSource(t, &fakeSidecar{tip: nil})
	_, err := src.Tip(t.Context())
	g.Expect(err).To(MatchError(ErrNoTip))
}

func TestSidecarSource_Digest(t *testing.T) {
	g := NewWithT(t)
	f := &fakeSidecar{outcomes: []fakeOutcome{{
		status: sidecar.Completed,
		result: json.RawMessage(`{"version":1,"final":{"count":7,"digest":"0xabc"}}`),
	}}}
	src := newTestSource(t, f)

	report, err := src.Digest(t.Context(), 80, BackendComposite)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(report.Version).To(Equal(int64(1)))
	g.Expect(report.Final.Count).To(Equal(uint64(7)))
	g.Expect(report.Final.Digest).To(Equal("0xabc"))

	g.Expect(f.submits).To(HaveLen(1))
	req := f.submits[0]
	g.Expect(req.Type).To(Equal(sidecar.TaskTypeEVMDigest))
	g.Expect(*req.Params).To(HaveKeyWithValue("height", float64(80)))
	g.Expect(*req.Params).To(HaveKeyWithValue("backend", BackendComposite))
	g.Expect(f.deleted).To(HaveLen(1))
}

func TestSidecarSource_DigestFailedTask(t *testing.T) {
	g := NewWithT(t)
	src := newTestSource(t, &fakeSidecar{outcomes: []fakeOutcome{{
		status: sidecar.Failed,
		err:    "changelog ends mid-record",
	}}})
	_, err := src.Digest(t.Context(), 80, BackendMemiavl)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("mid-record"))
}

func TestSidecarSource_DigestRetries(t *testing.T) {
	g := NewWithT(t)
	f := &fakeSidecar{outcomes: []fakeOutcome{
		{status: sidecar.Failed, err: "changelog ends mid-record"},
		{status: sidecar.Completed, result: json.RawMessage(`{"version":1,"final":{"count":1,"digest":"0x00"}}`)},
	}}
	src := newTestSource(t, f)
	src.Attempts = 2

	report, err := src.Digest(t.Context(), 80, BackendMemiavl)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(report.Final.Digest).To(Equal("0x00"))
	g.Expect(f.submits).To(HaveLen(2))
}

func TestSidecarSource_DigestBadResult(t *testing.T) {
	g := NewWithT(t)
	src := newTestSource(t, &fakeSidecar{outcomes: []fakeOutcome{{
		status: sidecar.Completed,
		result: json.RawMessage(`"not an object"`),
	}}})
	_, err := src.Digest(t.Context(), 80, BackendMemiavl)
	g.Expect(err).To(HaveOccurred())
}
