package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/auth"
	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/llm"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
	"github.com/transcend-software-labs/rasmus-ai/internal/orchestrator"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/storage"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/stream"
)

// newSourceTestServer builds a Server (internal package — handlers are called
// directly, bypassing requireAdmin, which has its own coverage) with a seeded
// project whose snapshot tarball sits in the returned storage.
func newSourceTestServer(t *testing.T) *Server {
	t.Helper()
	st := store.NewMemory()
	fake := llm.NewFake()
	machines := fly.NewFake()
	b := builder.NewSandbox(machines, func(string) opencode.Driver { return opencode.NewFake() }, builder.Config{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	assets := storage.NewMemory()
	orch := orchestrator.New(st, fake, fake, fake, b, machines, assets, stream.NewBroker(100), orchestrator.NoopVerifier{}, log)
	s, err := NewServer(config.Config{AdminEmail: "admin@example.com"}, st, auth.NewSessions(st, time.Hour), orch, stream.NewBroker(100), assets, log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"main.go":         "package main // hello reviewer\n",
		"static/logo.png": "\x89PNG\x00binary",
	} {
		_ = tw.WriteHeader(&tar.Header{Name: "./" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	ctx := context.Background()
	if err := assets.Put(ctx, "projects/p1/snapshot.tgz", "application/gzip", bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != nil {
		t.Fatalf("put snapshot: %v", err)
	}
	now := time.Now().UTC()
	if err := st.CreateProject(ctx, &project.Project{
		ID: "p1", UserID: "u1", Name: "Bakery", Status: project.StatusAccepted,
		SnapshotKey: "projects/p1/snapshot.tgz", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return s
}

func sourceGet(s *Server, target, id, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.SetPathValue("id", id)
	if path != "" {
		r.SetPathValue("path", path)
	}
	w := httptest.NewRecorder()
	if strings.HasSuffix(target, ".tgz") {
		s.handleAdminSourceDownload(w, r, nil)
	} else {
		s.handleAdminSource(w, r, nil)
	}
	return w
}

func TestAdminSource_ListViewDownload(t *testing.T) {
	s := newSourceTestServer(t)

	// Listing: every file present, linked.
	w := sourceGet(s, "/admin/projects/p1/source", "p1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	out := w.Body.String()
	for _, want := range []string{"main.go", "static/logo.png", "/admin/projects/p1/source.tgz"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing missing %q", want)
		}
	}

	// A text file renders inline.
	w = sourceGet(s, "/admin/projects/p1/source/main.go", "p1", "main.go")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "hello reviewer") {
		t.Errorf("file view: status %d, want the file contents", w.Code)
	}

	// A binary file is handed over as a download, not rendered.
	w = sourceGet(s, "/admin/projects/p1/source/static/logo.png", "p1", "static/logo.png")
	if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, "logo.png") {
		t.Errorf("binary file should download, got disposition %q", got)
	}

	// Unknown path → 404.
	if w = sourceGet(s, "/admin/projects/p1/source/nope.go", "p1", "nope.go"); w.Code != http.StatusNotFound {
		t.Errorf("missing file status = %d, want 404", w.Code)
	}

	// The whole tarball downloads with a stable name.
	w = sourceGet(s, "/admin/projects/p1/source.tgz", "p1", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Disposition"), "forge-p1-source.tgz") {
		t.Errorf("download: status %d, disposition %q", w.Code, w.Header().Get("Content-Disposition"))
	}
}

func TestAdminSource_NoSnapshotIs404(t *testing.T) {
	s := newSourceTestServer(t)
	now := time.Now().UTC()
	_ = s.store.CreateProject(context.Background(), &project.Project{
		ID: "p2", UserID: "u1", Name: "Empty", Status: project.StatusCreated, CreatedAt: now, UpdatedAt: now,
	})
	if w := sourceGet(s, "/admin/projects/p2/source", "p2", ""); w.Code != http.StatusNotFound {
		t.Errorf("no-snapshot listing status = %d, want 404", w.Code)
	}
	if w := sourceGet(s, "/admin/projects/p2/source.tgz", "p2", ""); w.Code != http.StatusNotFound {
		t.Errorf("no-snapshot download status = %d, want 404", w.Code)
	}
}
