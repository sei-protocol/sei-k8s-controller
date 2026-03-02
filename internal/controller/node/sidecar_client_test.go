package node

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewSidecarClient_URLConstruction(t *testing.T) {
	tests := []struct {
		name      string
		nodeName  string
		namespace string
		port      int32
		wantURL   string
	}{
		{
			name:      "explicit port",
			nodeName:  "sei-atlantic2",
			namespace: "sei",
			port:      8080,
			wantURL:   "http://sei-atlantic2-0.sei-atlantic2.sei.svc.cluster.local:8080",
		},
		{
			name:      "zero port defaults to 7777",
			nodeName:  "sei-pacific1",
			namespace: "default",
			port:      0,
			wantURL:   "http://sei-pacific1-0.sei-pacific1.default.svc.cluster.local:7777",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := NewSidecarClient(tt.nodeName, tt.namespace, tt.port)
			if sc.baseURL != tt.wantURL {
				t.Errorf("baseURL = %q, want %q", sc.baseURL, tt.wantURL)
			}
		})
	}
}

func TestSidecarClient_Status(t *testing.T) {
	want := StatusResponse{
		Phase:           "Ready",
		CurrentTask:     "",
		LastTask:        "mark-ready",
		LastTaskResult:  "success",
		BlockHeight:     195826001,
		CatchingUp:      false,
		PeerCount:       12,
		NodeID:          "abc123",
		UpgradeHeight:   0,
		UpgradeImage:    "",
		PendingUpgrades: 1,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/status" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(want) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	sc := &SidecarClient{baseURL: srv.URL, httpClient: srv.Client()}
	got, err := sc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if *got != want {
		t.Errorf("Status() = %+v, want %+v", *got, want)
	}
}

func TestSidecarClient_Status_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	sc := &SidecarClient{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := sc.Status(context.Background())
	if err == nil {
		t.Fatal("Status() expected error for 500 response")
	}
}

func TestSidecarClient_SubmitTask_Accepted(t *testing.T) {
	var received TaskRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/task" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	sc := &SidecarClient{baseURL: srv.URL, httpClient: srv.Client()}
	task := TaskRequest{
		Type:   "snapshot-restore",
		Params: map[string]any{"bucket": "my-bucket", "prefix": "snapshots/latest.tar"},
	}
	if err := sc.SubmitTask(context.Background(), task); err != nil {
		t.Fatalf("SubmitTask() error = %v", err)
	}
	if received.Type != task.Type {
		t.Errorf("server received type %q, want %q", received.Type, task.Type)
	}
}

func TestSidecarClient_SubmitTask_Busy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "task already running", http.StatusConflict)
	}))
	defer srv.Close()

	sc := &SidecarClient{baseURL: srv.URL, httpClient: srv.Client()}
	err := sc.SubmitTask(context.Background(), TaskRequest{Type: "config-patch"})
	if !errors.Is(err, ErrSidecarBusy) {
		t.Errorf("SubmitTask() error = %v, want ErrSidecarBusy", err)
	}
}

func TestSidecarClient_SubmitTask_UnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	sc := &SidecarClient{baseURL: srv.URL, httpClient: srv.Client()}
	err := sc.SubmitTask(context.Background(), TaskRequest{Type: "config-patch"})
	if err == nil {
		t.Fatal("SubmitTask() expected error for 400 response")
	}
	if errors.Is(err, ErrSidecarBusy) {
		t.Error("SubmitTask() should not return ErrSidecarBusy for 400")
	}
}

func TestSidecarClient_Healthz_Ready(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sc := &SidecarClient{baseURL: srv.URL, httpClient: srv.Client()}
	ready, err := sc.Healthz(context.Background())
	if err != nil {
		t.Fatalf("Healthz() error = %v", err)
	}
	if !ready {
		t.Error("Healthz() = false, want true")
	}
}

func TestSidecarClient_Healthz_NotReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	sc := &SidecarClient{baseURL: srv.URL, httpClient: srv.Client()}
	ready, err := sc.Healthz(context.Background())
	if err != nil {
		t.Fatalf("Healthz() error = %v", err)
	}
	if ready {
		t.Error("Healthz() = true, want false")
	}
}

func TestSidecarClient_Healthz_UnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "teapot", http.StatusTeapot)
	}))
	defer srv.Close()

	sc := &SidecarClient{baseURL: srv.URL, httpClient: srv.Client()}
	ready, err := sc.Healthz(context.Background())
	if err == nil {
		t.Fatal("Healthz() expected error for 418 response")
	}
	if ready {
		t.Error("Healthz() = true on error, want false")
	}
}

func TestSidecarClient_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Use a very short timeout to trigger a timeout error.
	sc := &SidecarClient{
		baseURL:    srv.URL,
		httpClient: &http.Client{Timeout: 10 * time.Millisecond},
	}

	_, err := sc.Status(context.Background())
	if err == nil {
		t.Fatal("Status() expected timeout error")
	}

	err = sc.SubmitTask(context.Background(), TaskRequest{Type: "mark-ready"})
	if err == nil {
		t.Fatal("SubmitTask() expected timeout error")
	}

	_, err = sc.Healthz(context.Background())
	if err == nil {
		t.Fatal("Healthz() expected timeout error")
	}
}
