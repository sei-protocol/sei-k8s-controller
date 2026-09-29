package server

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

// Hash log file names as written by seid's hash logger: sealed files are
// {index}-{first}-{last}-{version}.hlog, the file being written is
// {index}-{version}.hlog.u.
var (
	sealedHashLogName   = regexp.MustCompile(`^(\d+)-\d+-\d+-[A-Za-z0-9._]+\.hlog$`)
	unsealedHashLogName = regexp.MustCompile(`^(\d+)-[A-Za-z0-9._]+\.hlog\.u$`)
)

// hashLogDir is seid's default hash log directory for this home.
func (s *Server) hashLogDir() string {
	return filepath.Join(s.homeDir, "data", "hash.log")
}

// parseHashLogName reports the file index and whether name is a sealed hash
// log file. ok is false for anything that is not a hash log file name.
func parseHashLogName(name string) (index uint64, sealed, ok bool) {
	m := sealedHashLogName.FindStringSubmatch(name)
	if m != nil {
		sealed = true
	} else if m = unsealedHashLogName.FindStringSubmatch(name); m == nil {
		return 0, false, false
	}
	index, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, false, false
	}
	return index, sealed, true
}

// HashLogFile describes one file in the node's hash log directory. It matches
// the HashLogFile schema in sidecarapi/api/openapi.yaml.
type HashLogFile struct {
	Name   string `json:"name"`
	Index  uint64 `json:"index"`
	Size   int64  `json:"size"`
	Sealed bool   `json:"sealed"`
}

// HashLogListResponse is the JSON body for GET /v0/hashlog.
type HashLogListResponse struct {
	Files []HashLogFile `json:"files"`
}

// handleListHashLog lists the node's hash log files in file-index order.
func (s *Server) handleListHashLog(w http.ResponseWriter, _ *http.Request) {
	entries, err := os.ReadDir(s.hashLogDir())
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "hash log directory not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading hash log directory: "+err.Error())
		return
	}

	files := make([]HashLogFile, 0, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		index, sealed, ok := parseHashLogName(e.Name())
		if !ok {
			continue
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // sealed or pruned between ReadDir and Info
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "reading hash log file info: "+err.Error())
			return
		}
		files = append(files, HashLogFile{Name: e.Name(), Index: index, Size: info.Size(), Sealed: sealed})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Index < files[j].Index })
	writeJSON(w, http.StatusOK, HashLogListResponse{Files: files})
}

// handleGetHashLogFile serves one hash log file's raw bytes. Range requests
// are honored so a caller can read only what was appended since its last read.
func (s *Server) handleGetHashLogFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, _, ok := parseHashLogName(name); !ok {
		writeError(w, http.StatusBadRequest, "not a hash log file name")
		return
	}

	f, err := os.Open(filepath.Join(s.hashLogDir(), name))
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "hash log file not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "opening hash log file: "+err.Error())
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading hash log file info: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	http.ServeContent(w, r, "", info.ModTime(), f)
}
