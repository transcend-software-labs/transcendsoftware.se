package github

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/nacl/box"
)

// apiBase is the GitHub REST API root.
const apiBase = "https://api.github.com"

// HTTP is a real Mirror against the GitHub REST API.
type HTTP struct {
	org     string
	token   string
	apiBase string
	webBase string
	log     *slog.Logger
	client  *http.Client
}

// Options configures the real mirror.
type Options struct {
	Org     string
	Token   string
	APIBase string // optional; defaults to api.github.com (overridable in tests)
	WebBase string // optional; defaults to https://github.com
	Logger  *slog.Logger
}

// NewHTTP returns a real GitHub mirror.
func NewHTTP(o Options) *HTTP {
	api := o.APIBase
	if api == "" {
		api = apiBase
	}
	web := o.WebBase
	if web == "" {
		web = "https://github.com"
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &HTTP{
		org: o.Org, token: o.Token, apiBase: api, webBase: web, log: log,
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

func (h *HTTP) Push(ctx context.Context, spec PushSpec) (string, error) {
	if err := h.ensureRepo(ctx, spec.Repo); err != nil {
		return "", fmt.Errorf("ensure repo: %w", err)
	}
	if spec.FlyToken != "" {
		if err := h.setSecret(ctx, spec.Repo, "FLY_API_TOKEN", spec.FlyToken); err != nil {
			return "", fmt.Errorf("set secret: %w", err)
		}
	}
	if err := h.commitFiles(ctx, spec.Repo, spec.Message, spec.Files); err != nil {
		return "", fmt.Errorf("commit files: %w", err)
	}
	return h.webBase + "/" + h.org + "/" + spec.Repo, nil
}

// ensureRepo creates the private repo if it doesn't exist, waiting until its
// git backend is ready.
func (h *HTTP) ensureRepo(ctx context.Context, repo string) error {
	if err := h.do(ctx, http.MethodGet, "/repos/"+h.org+"/"+repo, nil, nil); err == nil {
		return nil
	}
	// auto_init:true gives the repo an initial commit on main, so the Git Data
	// API (blobs/trees) works — it 409s on a truly empty repo. Our first real
	// commit replaces the whole tree, so the auto README doesn't linger.
	if err := h.do(ctx, http.MethodPost, "/orgs/"+h.org+"/repos", map[string]any{
		"name": repo, "private": true, "auto_init": true,
		"description": "Built by Transcend Forge",
	}, nil); err != nil {
		return err
	}
	// Repo creation + initial commit are async; the git data endpoints 404 for
	// a moment. Wait until main exists before pushing.
	base := "/repos/" + h.org + "/" + repo
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := h.do(ctx, http.MethodGet, base+"/git/ref/heads/main", nil, nil); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("repo %q git backend not ready", repo)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// commitFiles pushes files as one commit on main. The tree includes
// .github/workflows/deploy.yml, which GitHub rejects WHOLESALE (404) if the
// token can't write workflows (classic PATs: the `workflow` scope; fine-grained:
// the "Workflows" permission). Rather than mirror NOTHING in that case, retry
// with the workflow files dropped so the source still lands — the deploy
// workflow just won't be wired until the token scope is fixed. The warning makes
// the degraded state visible instead of a silent empty repo.
func (h *HTTP) commitFiles(ctx context.Context, repo, message string, files map[string][]byte) error {
	if err := h.pushCommit(ctx, repo, message, files); err == nil {
		return nil
	} else {
		src, wf := splitWorkflowFiles(files)
		if len(wf) == 0 || len(src) == 0 {
			return err // nothing to drop, or nothing left to push — a real failure
		}
		h.log.Warn("mirror: push with workflow files failed; retrying source-only "+
			"(token likely lacks workflow-write — deploy workflow will be missing)",
			"repo", repo, "err", err)
		return h.pushCommit(ctx, repo, message, src)
	}
}

// splitWorkflowFiles partitions files into those under .github/workflows/ (which
// need workflow-write) and everything else (needs only contents-write).
func splitWorkflowFiles(files map[string][]byte) (src, wf map[string][]byte) {
	src = make(map[string][]byte)
	wf = make(map[string][]byte)
	for p, b := range files {
		if strings.HasPrefix(p, ".github/workflows/") {
			wf[p] = b
		} else {
			src[p] = b
		}
	}
	return src, wf
}

// pushCommit writes every file as one commit advancing main via the Git Data
// API: a blob per file (base64, so binary is fine), one tree, one commit, then
// move the ref.
func (h *HTTP) pushCommit(ctx context.Context, repo, message string, files map[string][]byte) error {
	base := "/repos/" + h.org + "/" + repo

	type treeEntry struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	entries := make([]treeEntry, 0, len(files))
	for _, path := range sortedKeys(files) {
		var blob struct {
			SHA string `json:"sha"`
		}
		if err := h.do(ctx, http.MethodPost, base+"/git/blobs", map[string]any{
			"content":  base64.StdEncoding.EncodeToString(files[path]),
			"encoding": "base64",
		}, &blob); err != nil {
			return err
		}
		entries = append(entries, treeEntry{Path: path, Mode: "100644", Type: "blob", SHA: blob.SHA})
	}

	// auto_init guarantees main exists, so we always have a parent.
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := h.do(ctx, http.MethodGet, base+"/git/ref/heads/main", nil, &ref); err != nil {
		return err
	}

	// Create the tree, retrying briefly: freshly-created blobs take a moment to
	// become referenceable.
	var tree struct {
		SHA string `json:"sha"`
	}
	var err error
	for attempt := 1; attempt <= 5; attempt++ {
		if err = h.do(ctx, http.MethodPost, base+"/git/trees",
			map[string]any{"tree": entries}, &tree); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		return err
	}

	var commit struct {
		SHA string `json:"sha"`
	}
	if err := h.do(ctx, http.MethodPost, base+"/git/commits", map[string]any{
		"message": message, "tree": tree.SHA, "parents": []string{ref.Object.SHA},
	}, &commit); err != nil {
		return err
	}
	return h.do(ctx, http.MethodPatch, base+"/git/refs/heads/main",
		map[string]any{"sha": commit.SHA, "force": true}, nil)
}

// setSecret creates/updates an Actions repo secret (sealed-box encrypted with
// the repo's public key, per GitHub's API).
func (h *HTTP) setSecret(ctx context.Context, repo, name, value string) error {
	base := "/repos/" + h.org + "/" + repo
	var key struct {
		KeyID string `json:"key_id"`
		Key   string `json:"key"`
	}
	if err := h.do(ctx, http.MethodGet, base+"/actions/secrets/public-key", nil, &key); err != nil {
		return err
	}
	enc, err := sealSecret(key.Key, value)
	if err != nil {
		return err
	}
	return h.do(ctx, http.MethodPut, base+"/actions/secrets/"+name, map[string]any{
		"encrypted_value": enc, "key_id": key.KeyID,
	}, nil)
}

// sealSecret encrypts value for a base64 NaCl public key using libsodium's
// crypto_box_seal (what GitHub Actions secrets require).
func sealSecret(pubKeyB64, value string) (string, error) {
	pk, err := base64.StdEncoding.DecodeString(pubKeyB64)
	if err != nil || len(pk) != 32 {
		return "", fmt.Errorf("bad public key")
	}
	var recipient [32]byte
	copy(recipient[:], pk)

	ephPub, ephPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	// nonce = blake2b(ephemeralPub || recipientPub), 24 bytes.
	hsh, err := blake2b.New(24, nil)
	if err != nil {
		return "", err
	}
	hsh.Write(ephPub[:])
	hsh.Write(recipient[:])
	var nonce [24]byte
	copy(nonce[:], hsh.Sum(nil))

	// crypto_box_seal = ephemeralPub || box(message). box.Seal prepends by
	// using ephPub[:] as the output prefix.
	sealed := box.Seal(ephPub[:], []byte(value), &nonce, &recipient, ephPriv)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (h *HTTP) do(ctx context.Context, method, path string, in, out any) error {
	var reader io.Reader
	if in != nil {
		body, err := json.Marshal(in)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.apiBase+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "transcend-forge")
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("github: %s %s -> %d: %s", method, path, resp.StatusCode, truncate(raw))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func truncate(b []byte) string {
	if len(b) > 300 {
		return string(b[:300])
	}
	return string(b)
}
