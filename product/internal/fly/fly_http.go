package fly

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// machinesAPI is the Fly Machines API base.
const machinesAPI = "https://api.machines.dev/v1"

// HTTP is a real Fly client (Machines + Apps API).
type HTTP struct {
	token        string // Fly API token (org — trusted side only, never the sandbox)
	org          string // org slug for app creation
	deployToken  string // deploy-scoped token injected into the sandbox for `fly deploy`
	sandboxApp   string // Fly app the per-task sandbox machines run under
	sandboxImage string // OCI image containing opencode + toolchains
	client       *http.Client
}

// Options configures the real Fly client.
type Options struct {
	Token        string
	Org          string
	DeployToken  string
	SandboxApp   string
	SandboxImage string
}

// NewHTTP returns a real Fly client.
func NewHTTP(o Options) *HTTP {
	return &HTTP{
		token:        o.Token,
		org:          o.Org,
		deployToken:  o.DeployToken,
		sandboxApp:   o.SandboxApp,
		sandboxImage: o.SandboxImage,
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
// Interim: returns the configured deploy-scoped token. It is a limited token
// (deploy operations only, no org admin or secret reads), never the org API
// token. But it is scoped to the *org*, so a misbehaving or prompt-injected
// build agent could deploy to (or destroy) any app in the org, not just its own.
//
// Hardening TODO: mint a token scoped to appName alone, per task, with a short
// expiry (~1h), and let it expire after the build. The mechanism is Fly's
// `createLimitedAccessToken` GraphQL mutation (what `fly tokens create deploy
// -a <app>` calls): profile "deploy", the app as the resource, organizationId
// from the org. The blocker is authority: minting sub-tokens requires an
// org-privileged token — a deploy-scoped token like this one gets "Not
// authorized to access this createlimitedaccesstoken". So enabling per-app
// scoping means giving the (trusted) orchestrator a token that can mint tokens,
// which is a deliberate trade-off to make explicitly, not silently.
func (h *HTTP) AppDeployToken(_ context.Context, _ string) (string, error) {
	return h.deployToken, nil
}

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
		return nil, err
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
// A non-zero exit code is returned in the result, not as an error.
func (h *HTTP) Exec(ctx context.Context, machineID string, command []string, timeoutSec int) (ExecResult, error) {
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	var out struct {
		ExitCode int32  `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	err := h.do(ctx, http.MethodPost,
		fmt.Sprintf("/apps/%s/machines/%s/exec", h.sandboxApp, machineID),
		map[string]any{"command": command, "timeout": timeoutSec}, &out)
	if err != nil {
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
