package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestListHashLog_OK(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/hashlog" || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(HashLogListResponse{Files: []HashLogFile{
			{Name: "3-v6.7.0.hlog.u", Index: 3, Size: 42},
		}})
	}))

	files, err := c.ListHashLog(context.Background())
	if err != nil {
		t.Fatalf("ListHashLog() error = %v", err)
	}
	if len(files) != 1 || files[0].Name != "3-v6.7.0.hlog.u" || files[0].Size != 42 {
		t.Errorf("files = %+v", files)
	}
}

func TestListHashLog_NotFound(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"hash log directory not found"}`))
	}))

	if _, err := c.ListHashLog(context.Background()); !errors.Is(err, ErrHashLogNotFound) {
		t.Fatalf("err = %v, want ErrHashLogNotFound", err)
	}
}

func TestReadHashLogFile_SendsRangeFromOffset(t *testing.T) {
	var gotRange string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/hashlog/3-v6.7.0.hlog.u" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		gotRange = r.Header.Get("Range")
		w.Header().Set("Content-Type", "text/csv")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("2,bb\n"))
	}))

	data, err := c.ReadHashLogFile(context.Background(), "3-v6.7.0.hlog.u", 28)
	if err != nil {
		t.Fatalf("ReadHashLogFile() error = %v", err)
	}
	if gotRange != "bytes=28-" {
		t.Errorf("Range = %q, want bytes=28-", gotRange)
	}
	if string(data) != "2,bb\n" {
		t.Errorf("data = %q", data)
	}
}

func TestReadHashLogFile_ZeroOffsetSendsNoRange(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "" {
			t.Errorf("Range = %q, want none", got)
		}
		_, _ = w.Write([]byte("block_number\n"))
	}))

	data, err := c.ReadHashLogFile(context.Background(), "0-v6.7.0.hlog.u", 0)
	if err != nil || string(data) != "block_number\n" {
		t.Fatalf("data = %q, err = %v", data, err)
	}
}

func TestReadHashLogFile_RangePastEndReturnsNoBytes(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))

	data, err := c.ReadHashLogFile(context.Background(), "0-v6.7.0.hlog.u", 100)
	if err != nil || len(data) != 0 {
		t.Fatalf("data = %q, err = %v", data, err)
	}
}

func TestReadHashLogFile_NotFound(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"hash log file not found"}`))
	}))

	if _, err := c.ReadHashLogFile(context.Background(), "0-v6.7.0.hlog.u", 0); !errors.Is(err, ErrHashLogNotFound) {
		t.Fatalf("err = %v, want ErrHashLogNotFound", err)
	}
}

func TestReadHashLogHead_SendsBoundedRange(t *testing.T) {
	var gotRange string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("block_number,"))
	}))

	data, err := c.ReadHashLogHead(context.Background(), "0-v6.7.0.hlog.u", 13)
	if err != nil || string(data) != "block_number," {
		t.Fatalf("data = %q, err = %v", data, err)
	}
	if gotRange != "bytes=0-12" {
		t.Errorf("Range = %q, want bytes=0-12", gotRange)
	}
}

func TestReadHashLogHead_TruncatesFullResponse(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("block_number,blockHash\n1,aa\n"))
	}))

	data, err := c.ReadHashLogHead(context.Background(), "0-v6.7.0.hlog.u", 5)
	if err != nil || string(data) != "block" {
		t.Fatalf("data = %q, err = %v", data, err)
	}
}
