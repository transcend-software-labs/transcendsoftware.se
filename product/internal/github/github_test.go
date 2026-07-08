package github

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// mockGitHub is a tiny stand-in for the GitHub REST API covering the mirror flow.
func mockGitHub(t *testing.T) (*httptest.Server, *mockState) {
	t.Helper()
	st := &mockState{files: map[string]string{}, secrets: map[string]bool{}}
	// A base64 NaCl public key (any valid 32-byte key works for sealing).
	st.pubKey = base64.StdEncoding.EncodeToString(make([]byte, 32))

	mux := http.NewServeMux()
	// repo exists check / create
	mux.HandleFunc("/repos/acme/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/git/ref/heads/main") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"object":{"sha":"parentsha"}}`))
		case strings.HasSuffix(p, "/git/blobs"):
			_, _ = w.Write([]byte(`{"sha":"blobsha"}`))
		case strings.HasSuffix(p, "/git/trees"):
			body, _ := io.ReadAll(r.Body)
			// Simulate GitHub rejecting a tree that touches .github/workflows/
			// when the token lacks workflow-write.
			if st.rejectWorkflowTree && strings.Contains(string(body), ".github/workflows/") {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			st.mu.Lock()
			st.lastTree = string(body)
			st.mu.Unlock()
			_, _ = w.Write([]byte(`{"sha":"treesha"}`))
		case strings.HasSuffix(p, "/git/commits"):
			_, _ = w.Write([]byte(`{"sha":"commitsha"}`))
		case strings.HasSuffix(p, "/git/refs/heads/main") && r.Method == http.MethodPatch:
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(p, "/actions/secrets/public-key"):
			_, _ = w.Write([]byte(`{"key_id":"kid","key":"` + st.pubKey + `"}`))
		case strings.Contains(p, "/actions/secrets/") && r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			st.mu.Lock()
			st.secrets[strings.TrimPrefix(p, "/repos/acme/testrepo/actions/secrets/")] = strings.Contains(string(body), "encrypted_value")
			st.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet: // repo exists
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	srv := httptest.NewServer(mux)
	return srv, st
}

type mockState struct {
	mu                 sync.Mutex
	files              map[string]string
	secrets            map[string]bool
	pubKey             string
	rejectWorkflowTree bool   // 404 any tree containing .github/workflows/
	lastTree           string // body of the last accepted tree POST
}

func TestPush_FullFlow(t *testing.T) {
	srv, st := mockGitHub(t)
	defer srv.Close()
	m := NewHTTP(Options{Org: "acme", Token: "t", APIBase: srv.URL, WebBase: "https://github.com"})

	url, err := m.Push(context.Background(), PushSpec{
		Repo:    "testrepo",
		Message: "Build",
		Files: map[string][]byte{
			"index.html":                   []byte("<h1>hi</h1>"),
			".github/workflows/deploy.yml": []byte("name: Deploy"),
		},
		FlyToken: "FlyV1 deploy-token",
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if url != "https://github.com/acme/testrepo" {
		t.Errorf("unexpected url %q", url)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.secrets["FLY_API_TOKEN"] {
		t.Error("FLY_API_TOKEN secret was not set (encrypted)")
	}
}

// When the token can't write workflows, GitHub 404s the whole tree. The mirror
// must degrade to pushing the source without the workflow file — not lose
// everything (which previously left an empty repo with only its auto README).
func TestPush_DegradesWhenWorkflowRejected(t *testing.T) {
	srv, st := mockGitHub(t)
	defer srv.Close()
	st.rejectWorkflowTree = true
	m := NewHTTP(Options{Org: "acme", Token: "t", APIBase: srv.URL, WebBase: "https://github.com"})

	url, err := m.Push(context.Background(), PushSpec{
		Repo:    "testrepo",
		Message: "Build",
		Files: map[string][]byte{
			"index.html":                   []byte("<h1>hi</h1>"),
			".github/workflows/deploy.yml": []byte("name: Deploy"),
		},
	})
	if err != nil {
		t.Fatalf("push should succeed by degrading to source-only, got: %v", err)
	}
	if url != "https://github.com/acme/testrepo" {
		t.Errorf("unexpected url %q", url)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !strings.Contains(st.lastTree, "index.html") {
		t.Errorf("source file not in final tree: %s", st.lastTree)
	}
	if strings.Contains(st.lastTree, ".github/workflows/") {
		t.Errorf("workflow file should have been dropped from final tree: %s", st.lastTree)
	}
}

func TestSealSecret_Deterministic(t *testing.T) {
	// A valid 32-byte key seals without error and produces base64 output.
	pub := base64.StdEncoding.EncodeToString(make([]byte, 32))
	out, err := sealSecret(pub, "hello")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		t.Fatalf("output not base64: %v", err)
	}
	// sealed = 32-byte ephemeral pubkey + 16-byte poly1305 tag + len(message)
	if len(raw) != 32+16+len("hello") {
		t.Errorf("unexpected sealed length %d", len(raw))
	}
}
