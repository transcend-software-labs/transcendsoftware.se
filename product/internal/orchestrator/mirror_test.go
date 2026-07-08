package orchestrator

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/github"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
)

func gzTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(content))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestMirrorToGitHub_PushesSourceOnly(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	mirror := github.NewFake()
	orch.SetMirror(mirror)

	// A snapshot tarball in storage, as a real build would have left.
	key := "projects/p1/snapshot.tgz"
	if err := orch.storage.Put(context.Background(), key, "application/gzip",
		bytes.NewReader(gzTar(t, map[string]string{
			"./index.html":        "<h1>hi</h1>",
			"./src/pages/a.astro": "a",
			"./.git/config":       "should be skipped",
		})), 0); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	p := &project.Project{ID: "p1", UserID: "u1", Name: "Bakery", Status: project.StatusPreviewReady,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	_ = st.CreateProject(context.Background(), p)

	orch.mirrorToGitHub(context.Background(), p, key, "Build: Bakery")

	if len(mirror.Pushes) != 1 {
		t.Fatalf("expected one push, got %d", len(mirror.Pushes))
	}
	push := mirror.Pushes[0]
	if _, ok := push.Files["index.html"]; !ok {
		t.Error("index.html missing from push")
	}
	if _, ok := push.Files["src/pages/a.astro"]; !ok {
		t.Error("nested file missing from push")
	}
	if _, ok := push.Files[".git/config"]; ok {
		t.Error(".git files must be skipped")
	}
	// Source-only mirror: no deploy-on-push Action (the agent already deployed).
	if _, ok := push.Files[".github/workflows/deploy.yml"]; ok {
		t.Error("deploy workflow must NOT be pushed — the mirror is source-only")
	}
	if push.FlyToken != "" {
		t.Error("no FLY_API_TOKEN should be set — there is no deploy Action to use it")
	}
	// RepoURL persisted on the project.
	got, _ := st.ProjectByID(context.Background(), "p1")
	if !strings.Contains(got.RepoURL, "forge-p1") {
		t.Errorf("repo URL not persisted, got %q", got.RepoURL)
	}
}
