package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// The operator's window into a generated site's source. The only copy of the
// code is the workspace snapshot (projects/<id>/snapshot.tgz) the build agent
// uploads after each pass — these handlers read that tarball on demand, so
// there is nothing to keep in sync and nothing extra to store.

// sourceFile is one entry of the snapshot tarball.
type sourceFile struct {
	Path string
	Size int64
}

// adminSourceView backs both modes of the source page: the file listing
// (File == "") and a single file's contents.
type adminSourceView struct {
	Project *project.Project
	Files   []sourceFile

	File    string // path of the file being viewed ("" = listing)
	Content string // the file's text
}

// loadSnapshot fetches and parses a project's snapshot tarball. It returns the
// project, the sorted file list, and a lookup by path.
func (s *Server) loadSnapshot(r *http.Request) (*project.Project, []sourceFile, map[string][]byte, error) {
	p, err := s.store.ProjectByID(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, nil, nil, err
	}
	if p.SnapshotKey == "" {
		return p, nil, nil, errNoSnapshot
	}
	raw, err := s.storage.Get(r.Context(), p.SnapshotKey)
	if err != nil {
		return nil, nil, nil, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, nil, err
	}
	defer gz.Close()

	var files []sourceFile
	byPath := make(map[string][]byte)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name == "." || name == "" || strings.HasPrefix(name, "..") {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxSourceFileBytes+1))
		if err != nil {
			return nil, nil, nil, err
		}
		files = append(files, sourceFile{Path: name, Size: hdr.Size})
		byPath[name] = body
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return p, files, byPath, nil
}

var errNoSnapshot = errors.New("no snapshot for project")

// maxSourceFileBytes bounds a single viewed/served file: anything larger is a
// build artifact or binary asset, not source the operator reads in a browser.
const maxSourceFileBytes = 2 << 20 // 2 MiB

// handleAdminSource renders the snapshot's file listing, or one file when the
// rest of the URL names it (GET /admin/projects/{id}/source[/{path...}]).
func (s *Server) handleAdminSource(w http.ResponseWriter, r *http.Request, _ *user.User) {
	p, files, byPath, err := s.loadSnapshot(r)
	if errors.Is(err, errNoSnapshot) {
		http.Error(w, "no source snapshot yet — it appears after the first successful build", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("admin source: load snapshot", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	file := path.Clean(strings.TrimPrefix(r.PathValue("path"), "/"))
	if file == "." {
		file = ""
	}
	if file == "" {
		v := s.view(r, p.Name+" — source", adminSourceView{Project: p, Files: files})
		v.Lang = "en"
		s.render(w, http.StatusOK, "admin_source", v)
		return
	}

	body, ok := byPath[file]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if int64(len(body)) > maxSourceFileBytes || !isTextContent(body) {
		// Not something to render as a page — hand the bytes over as-is.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+path.Base(file)+`"`)
		_, _ = w.Write(body)
		return
	}
	v := s.view(r, p.Name+" — "+file, adminSourceView{Project: p, Files: files, File: file, Content: string(body)})
	v.Lang = "en"
	s.render(w, http.StatusOK, "admin_source", v)
}

// handleAdminSourceDownload streams the whole snapshot tarball
// (GET /admin/projects/{id}/source.tgz).
func (s *Server) handleAdminSourceDownload(w http.ResponseWriter, r *http.Request, _ *user.User) {
	p, err := s.store.ProjectByID(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if p.SnapshotKey == "" {
		http.Error(w, "no source snapshot yet — it appears after the first successful build", http.StatusNotFound)
		return
	}
	raw, err := s.storage.Get(r.Context(), p.SnapshotKey)
	if err != nil {
		s.log.Error("admin source: download snapshot", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="forge-`+p.ID+`-source.tgz"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	_, _ = w.Write(raw)
}

// isTextContent reports whether body renders sensibly as text: no NUL bytes
// and valid UTF-8 in the sniffed prefix.
func isTextContent(body []byte) bool {
	const sniffLen = 8192
	sniff := body
	truncated := false
	if len(sniff) > sniffLen {
		sniff, truncated = sniff[:sniffLen], true
	}
	if bytes.IndexByte(sniff, 0) >= 0 {
		return false
	}
	if truncated {
		// The sniff boundary may have cut a multibyte rune — trim at most one
		// rune's worth of bytes before judging validity.
		for i := 0; i < utf8.UTFMax-1 && len(sniff) > 0 && !utf8.Valid(sniff); i++ {
			sniff = sniff[:len(sniff)-1]
		}
	}
	return utf8.Valid(sniff)
}
