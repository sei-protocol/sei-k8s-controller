package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newHashLogServer(t *testing.T) (*Server, string) {
	t.Helper()
	home := t.TempDir()
	srv := NewServer("", newTestEngine(t, nil), home, AuthnModeUnauthenticated)
	return srv, filepath.Join(home, "data", "hash.log")
}

func writeHashLogFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestListHashLogOrdersByIndexAndSkipsOtherFiles(t *testing.T) {
	srv, dir := newHashLogServer(t)
	writeHashLogFile(t, dir, "10-2001-2500-v6.7.0.hlog", "a")
	writeHashLogFile(t, dir, "11-v6.7.0.hlog.u", "bb")
	writeHashLogFile(t, dir, "9-1-2000-v6.7.0.hlog", "ccc")
	writeHashLogFile(t, dir, "notes.txt", "x")
	if err := os.Mkdir(filepath.Join(dir, "12-v6.7.0.hlog.u"), 0o755); err != nil {
		t.Fatal(err)
	}

	rec := serveHTTP(srv, http.MethodGet, "/v0/hashlog", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var got HashLogListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []HashLogFile{
		{Name: "9-1-2000-v6.7.0.hlog", Index: 9, Size: 3, Sealed: true},
		{Name: "10-2001-2500-v6.7.0.hlog", Index: 10, Size: 1, Sealed: true},
		{Name: "11-v6.7.0.hlog.u", Index: 11, Size: 2, Sealed: false},
	}
	if len(got.Files) != len(want) {
		t.Fatalf("files = %+v, want %+v", got.Files, want)
	}
	for i := range want {
		if got.Files[i] != want[i] {
			t.Errorf("files[%d] = %+v, want %+v", i, got.Files[i], want[i])
		}
	}
}

func TestListHashLogMissingDirectoryReturns404(t *testing.T) {
	srv, _ := newHashLogServer(t)
	rec := serveHTTP(srv, http.MethodGet, "/v0/hashlog", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestGetHashLogFileServesRange(t *testing.T) {
	srv, dir := newHashLogServer(t)
	content := "block_number,changeset\n1,aa\n2,bb\n"
	writeHashLogFile(t, dir, "0-v6.7.0.hlog.u", content)

	rec := serveHTTP(srv, http.MethodGet, "/v0/hashlog/0-v6.7.0.hlog.u", "")
	if rec.Code != http.StatusOK || rec.Body.String() != content {
		t.Fatalf("full read: status %d, body %q", rec.Code, rec.Body)
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/hashlog/0-v6.7.0.hlog.u", nil)
	req.Header.Set("Range", "bytes=23-")
	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range read: status = %d, want 206", rec.Code)
	}
	if got := rec.Body.String(); got != "1,aa\n2,bb\n" {
		t.Errorf("range body = %q", got)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 23-32/33" {
		t.Errorf("Content-Range = %q", got)
	}
}

func TestGetHashLogFileRejectsNonHashLogNames(t *testing.T) {
	srv, dir := newHashLogServer(t)
	writeHashLogFile(t, dir, "notes.txt", "x")
	for _, name := range []string{"notes.txt", "..%2F..%2Fconfig%2Fnode_key.json", "0-v6.7.0.hlog.u.bak"} {
		rec := serveHTTP(srv, http.MethodGet, "/v0/hashlog/"+name, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}

func TestGetHashLogFileMissingReturns404(t *testing.T) {
	srv, dir := newHashLogServer(t)
	writeHashLogFile(t, dir, "0-v6.7.0.hlog.u", "x")
	rec := serveHTTP(srv, http.MethodGet, "/v0/hashlog/1-v6.7.0.hlog.u", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
