package fly

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/superfly/macaroon"
	"github.com/superfly/macaroon/flyio"
	"github.com/superfly/macaroon/resset"
)

// machinesAPI is the Fly Machines API base.
const machinesAPI = "https://api.machines.dev/v1"

// graphqlAPI is the Fly GraphQL endpoint (used to mint per-app deploy tokens).
const graphqlAPI = "https://api.fly.io/graphql"

// HTTP is a real Fly client (Machines + Apps + GraphQL APIs).
type HTTP struct {
	token        string // Fly API token (org — trusted side only, never the sandbox)
	org          string // org slug for app creation + token minting
	deployToken  string // fallback deploy token if per-app minting isn't available
	sandboxApp   string // Fly app the per-task sandbox machines run under
	sandboxImage string // OCI image containing opencode + toolchains
	graphqlURL   string // Fly GraphQL endpoint (overridable in tests)
	log          *slog.Logger
	client       *http.Client

	mu      sync.Mutex
	orgNode string // cached GraphQL node id of org
}

// Options configures the real Fly client.
type Options struct {
	Token        string
	Org          string
	DeployToken  string
	SandboxApp   string
	SandboxImage string
	GraphQLURL   string // optional; defaults to Fly's GraphQL endpoint
	Logger       *slog.Logger
}

// NewHTTP returns a real Fly client.
func NewHTTP(o Options) *HTTP {
	gql := o.GraphQLURL
	if gql == "" {
		gql = graphqlAPI
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &HTTP{
		token:        o.Token,
		org:          o.Org,
		deployToken:  o.DeployToken,
		sandboxApp:   o.SandboxApp,
		sandboxImage: o.SandboxImage,
		graphqlURL:   gql,
		log:          log,
		client:       &http.Client{Timeout: 120 * time.Second}, // covers the machine wait endpoint
	}
}

// EnsureApp creates the per-customer Fly app if it doesn't already exist.
func (h *HTTP) EnsureApp(ctx context.Context, appName string) error {
	// Already exists?
	if err := h.do(ctx, http.MethodGet, "/apps/"+appName, nil, nil); err == nil {
		return nil
	}
	// Create it under the configured org.
	return h.do(ctx, http.MethodPost, "/apps",
		map[string]any{"app_name": appName, "org_slug": h.org}, nil)
}

// AppDeployToken returns a token the sandbox agent uses to run `fly deploy`.
//
// It mints a token scoped to appName alone, per build, with a short expiry —
// so a prompt-injected or misbehaving agent can only deploy its own throwaway
// app, not anything else in the org. If minting isn't available (the runtime
// token can't create sub-tokens, or GraphQL is unreachable) it falls back to
// the configured org-scoped deploy token so builds keep working; that fallback
// is logged because it is a security downgrade.
func (h *HTTP) AppDeployToken(ctx context.Context, appName string) (string, error) {
	return h.scopedToken(ctx, appName, 2*time.Hour) // build-only; dies soon after
}

// RepoDeployToken returns a longer-lived app-scoped deploy token for the
// project's GitHub Action to redeploy on push. Refreshed on every build.
func (h *HTTP) RepoDeployToken(ctx context.Context, appName string) (string, error) {
	return h.scopedToken(ctx, appName, 365*24*time.Hour)
}

// scopedToken produces a deploy token restricted to appName with the given
// lifetime. Preferred path is local macaroon ATTENUATION of our own API token —
// pure computation, needs no minting authority (org tokens can't mint
// sub-tokens; only user sessions can). Falls back to the GraphQL mint (works
// if the runtime token is session-grade) and then to the configured org-wide
// deploy token, logging the downgrade.
func (h *HTTP) scopedToken(ctx context.Context, appName string, ttl time.Duration) (string, error) {
	tok, attErr := h.attenuatedToken(ctx, appName, ttl)
	if attErr == nil {
		return tok, nil
	}
	tok, mintErr := h.mintDeployToken(ctx, appName, fmt.Sprintf("%dh", int(ttl.Hours())))
	if mintErr == nil {
		return tok, nil
	}
	if h.deployToken != "" {
		h.log.Warn("fly: per-app token unavailable; using org-scoped fallback",
			"app", appName, "attenuate_err", attErr, "mint_err", mintErr)
		return h.deployToken, nil
	}
	return "", fmt.Errorf("fly: scoped token for %s: attenuate: %v; mint: %v", appName, attErr, mintErr)
}

// attenuatedToken narrows h.token (a Fly macaroon) to one app + a validity
// window, mirroring the caveat structure of official `fly tokens create
// deploy -a` tokens: full access to the app and the builder/wg features
// (remote builds need them), read-only for anything else present.
func (h *HTTP) attenuatedToken(ctx context.Context, appName string, ttl time.Duration) (string, error) {
	appID, err := h.appNumericID(ctx, appName)
	if err != nil {
		return "", err
	}
	bun, err := flyio.ParseBundle(h.token)
	if err != nil {
		return "", fmt.Errorf("parse token: %w", err)
	}
	now := time.Now()
	if err := bun.Attenuate(
		&resset.IfPresent{
			Ifs: macaroon.NewCaveatSet(
				&flyio.Apps{Apps: resset.New(resset.ActionAll, appID)},
				&flyio.FeatureSet{Features: resset.New(resset.ActionAll, "builder", "wg")},
			),
			Else: resset.ActionRead,
		},
		&macaroon.ValidityWindow{NotBefore: now.Add(-time.Minute).Unix(), NotAfter: now.Add(ttl).Unix()},
	); err != nil {
		return "", fmt.Errorf("attenuate: %w", err)
	}
	// flyctl and the APIs accept the header with the FlyV1 scheme.
	hdr := bun.String()
	if !strings.HasPrefix(hdr, "FlyV1 ") {
		hdr = "FlyV1 " + hdr
	}
	return hdr, nil
}

// appNumericID resolves an app's internal numeric id (what app caveats key on).
func (h *HTTP) appNumericID(ctx context.Context, appName string) (uint64, error) {
	var out struct {
		InternalNumericID uint64 `json:"internal_numeric_id"`
	}
	if err := h.do(ctx, http.MethodGet, "/apps/"+appName, nil, &out); err != nil {
		return 0, err
	}
	if out.InternalNumericID == 0 {
		return 0, fmt.Errorf("app %q has no numeric id", appName)
	}
	return out.InternalNumericID, nil
}

// SetAppSecrets sets runtime secrets on a per-customer app via the Fly GraphQL
// setSecrets mutation. The values apply on the app's next deploy — which, in
// the build flow, is the agent's deploy right after this call.
func (h *HTTP) SetAppSecrets(ctx context.Context, appName string, secrets map[string]string) error {
	if len(secrets) == 0 {
		return nil
	}
	type kv struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	list := make([]kv, 0, len(secrets))
	for k, v := range secrets {
		list = append(list, kv{Key: k, Value: v})
	}
	const mutation = `mutation($input: SetSecretsInput!){setSecrets(input:$input){app{name}}}`
	vars := map[string]any{"input": map[string]any{"appId": appName, "secrets": list}}
	var out struct {
		SetSecrets struct {
			App struct {
				Name string `json:"name"`
			} `json:"app"`
		} `json:"setSecrets"`
	}
	return h.graphql(ctx, mutation, vars, &out)
}

// mintDeployToken creates a Fly deploy token scoped to appName (the
// createLimitedAccessToken mutation that `fly tokens create deploy -a` uses).
func (h *HTTP) mintDeployToken(ctx context.Context, appName, expiry string) (string, error) {
	orgNode, err := h.orgID(ctx)
	if err != nil {
		return "", err
	}
	const mutation = `mutation($input: CreateLimitedAccessTokenInput!){` +
		`createLimitedAccessToken(input:$input){limitedAccessToken{tokenHeader}}}`
	vars := map[string]any{"input": map[string]any{
		"name":           "forge-deploy-" + appName,
		"organizationId": orgNode,
		"profile":        "deploy",
		"profileParams":  map[string]any{"app_id": appName},
		"expiry":         expiry,
	}}
	var out struct {
		CreateLimitedAccessToken struct {
			LimitedAccessToken struct {
				TokenHeader string `json:"tokenHeader"`
			} `json:"limitedAccessToken"`
		} `json:"createLimitedAccessToken"`
	}
	if err := h.graphql(ctx, mutation, vars, &out); err != nil {
		return "", err
	}
	tok := out.CreateLimitedAccessToken.LimitedAccessToken.TokenHeader
	if tok == "" {
		return "", fmt.Errorf("empty token returned")
	}
	return tok, nil
}

// orgID resolves and caches the org's GraphQL node id.
func (h *HTTP) orgID(ctx context.Context) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.orgNode != "" {
		return h.orgNode, nil
	}
	var out struct {
		Organization struct {
			ID string `json:"id"`
		} `json:"organization"`
	}
	if err := h.graphql(ctx, `query($slug:String!){organization(slug:$slug){id}}`,
		map[string]any{"slug": h.org}, &out); err != nil {
		return "", err
	}
	if out.Organization.ID == "" {
		return "", fmt.Errorf("org %q not found", h.org)
	}
	h.orgNode = out.Organization.ID
	return h.orgNode, nil
}

// graphql runs a Fly GraphQL query/mutation, decoding data into out.
func (h *HTTP) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.graphqlURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("content-type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("fly graphql: decode (status %d): %w", resp.StatusCode, err)
	}
	if len(env.Errors) > 0 {
		return fmt.Errorf("fly graphql: %s", env.Errors[0].Message)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("fly graphql: status %d", resp.StatusCode)
	}
	return json.Unmarshal(env.Data, out)
}

// reStaleMachine matches Fly's 409 unique-machine-name violation and captures
// the squatting machine's id.
var reStaleMachine = regexp.MustCompile(`machine ID (\w+) already exists`)

func (h *HTTP) SpawnSandbox(ctx context.Context, spec SpawnSpec) (*Sandbox, error) {
	port := spec.Port
	if port == 0 {
		port = DefaultPort
	}
	env := map[string]string{}
	for k, v := range spec.Env {
		env[k] = v
	}
	env["OPENCODE_PORT"] = strconv.Itoa(port)

	payload := map[string]any{
		"name":   "sbx-" + strings.ToLower(spec.TaskID),
		"region": "arn",
		"config": map[string]any{
			"image":        h.sandboxImage,
			"guest":        map[string]any{"cpu_kind": "shared", "cpus": 2, "memory_mb": 2048},
			"env":          env,
			"auto_destroy": true,
		},
	}
	var created struct {
		ID        string `json:"id"`
		PrivateIP string `json:"private_ip"`
	}
	if err := h.do(ctx, http.MethodPost,
		fmt.Sprintf("/apps/%s/machines", h.sandboxApp), payload, &created); err != nil {
		// A stale sandbox from an interrupted build can still hold this
		// project's deterministic machine name (seen live as a 409 unique-name
		// violation). Destroy the squatter and retry once instead of failing
		// the build.
		m := reStaleMachine.FindStringSubmatch(err.Error())
		if m == nil {
			return nil, err
		}
		_ = h.DestroySandbox(ctx, &Sandbox{MachineID: m[1], AppName: h.sandboxApp})
		time.Sleep(3 * time.Second) // let the name release
		if err2 := h.do(ctx, http.MethodPost,
			fmt.Sprintf("/apps/%s/machines", h.sandboxApp), payload, &created); err2 != nil {
			return nil, err2
		}
	}

	sb := &Sandbox{MachineID: created.ID, AppName: h.sandboxApp}

	// Wait until the machine is started before returning a reachable address.
	// If any readiness step fails, destroy the machine we just created — leaving
	// it running would leak infrastructure until the reaper's slow sweep.
	if err := h.waitStarted(ctx, created.ID); err != nil {
		h.cleanupFailedSpawn(sb)
		return nil, err
	}

	// Reachable over Fly's private 6PN network (orchestrator must be on it).
	sb.Addr = fmt.Sprintf("http://[%s]:%d", created.PrivateIP, port)

	// The machine is "started" before opencode has bound its port; wait until it
	// actually accepts connections (else the first request is refused).
	if err := h.waitOpencodeReady(ctx, sb.Addr); err != nil {
		h.cleanupFailedSpawn(sb)
		return nil, err
	}
	return sb, nil
}

// cleanupFailedSpawn best-effort destroys a machine whose spawn didn't complete,
// using a fresh context so it runs even when the caller's ctx has been cancelled
// (a cancelled ctx is a common cause of the readiness wait failing).
func (h *HTTP) cleanupFailedSpawn(sb *Sandbox) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = h.DestroySandbox(ctx, sb)
}

// waitOpencodeReady polls the opencode address until it accepts connections.
func (h *HTTP) waitOpencodeReady(ctx context.Context, addr string) error {
	deadline := time.Now().Add(120 * time.Second)
	client := &http.Client{Timeout: 5 * time.Second}
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/", nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("fly: opencode not ready at %s: %w", addr, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// waitStarted polls the machine until it reaches the started state. (Fly's wait
// endpoint caps its timeout at 60s; polling handles a cold image pull cleanly.)
func (h *HTTP) waitStarted(ctx context.Context, machineID string) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var st struct {
			State string `json:"state"`
		}
		if err := h.do(ctx, http.MethodGet,
			fmt.Sprintf("/apps/%s/machines/%s", h.sandboxApp, machineID), nil, &st); err != nil {
			return fmt.Errorf("fly: poll machine state: %w", err)
		}
		switch st.State {
		case "started":
			return nil
		case "failed", "stopped", "destroyed":
			return fmt.Errorf("fly: machine entered state %q before starting", st.State)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("fly: timed out waiting for machine to start (last state %q)", st.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// DestroyApp deletes a per-customer app (machines, IPs, everything).
// Absent apps are treated as already destroyed.
func (h *HTTP) DestroyApp(ctx context.Context, appName string) error {
	err := h.do(ctx, http.MethodDelete, "/apps/"+appName, nil, nil)
	if err != nil && strings.Contains(err.Error(), "returned 404") {
		return nil // already gone — reaping is idempotent
	}
	return err
}

// SweepSandboxes destroys machines in the sandbox app older than olderThan.
// Builds are bounded by the pipeline timeout, so anything older is a leak.
func (h *HTTP) SweepSandboxes(ctx context.Context, olderThan time.Duration) (int, error) {
	var machines []struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := h.do(ctx, http.MethodGet,
		fmt.Sprintf("/apps/%s/machines", h.sandboxApp), nil, &machines); err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-olderThan)
	reaped := 0
	for _, m := range machines {
		if m.CreatedAt.After(cutoff) {
			continue
		}
		if err := h.do(ctx, http.MethodDelete,
			fmt.Sprintf("/apps/%s/machines/%s?force=true", h.sandboxApp, m.ID), nil, nil); err != nil {
			return reaped, err
		}
		reaped++
	}
	return reaped, nil
}

// Exec runs a command inside a sandbox machine via the Machines exec API.
// A non-zero exit code is returned in the result, not as an error. The request
// uses a client whose timeout tracks the exec timeout (the default 120s client
// would abort long execs like the multi-page screenshot crawl).
func (h *HTTP) Exec(ctx context.Context, machineID string, command []string, timeoutSec int) (ExecResult, error) {
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	body, err := json.Marshal(map[string]any{"command": command, "timeout": timeoutSec})
	if err != nil {
		return ExecResult{}, err
	}
	url := fmt.Sprintf("%s/apps/%s/machines/%s/exec", machinesAPI, h.sandboxApp, machineID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ExecResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("content-type", "application/json")
	client := &http.Client{Timeout: time.Duration(timeoutSec+30) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ExecResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return ExecResult{}, fmt.Errorf("fly: exec returned %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		ExitCode int32  `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return ExecResult{}, err
	}
	return ExecResult{ExitCode: out.ExitCode, Stdout: out.Stdout, Stderr: out.Stderr}, nil
}

func (h *HTTP) DestroySandbox(ctx context.Context, s *Sandbox) error {
	if s == nil || s.MachineID == "" {
		return nil
	}
	app := s.AppName
	if app == "" {
		app = h.sandboxApp // reaping by machine id only (e.g. startup recovery)
	}
	return h.do(ctx, http.MethodDelete,
		fmt.Sprintf("/apps/%s/machines/%s?force=true", app, s.MachineID), nil, nil)
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
	req, err := http.NewRequestWithContext(ctx, method, machinesAPI+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("content-type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("fly: %s %s returned %d: %s", method, path, resp.StatusCode, string(raw))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
