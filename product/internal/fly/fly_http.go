package fly

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// machinesAPI is the Fly Machines API base.
const machinesAPI = "https://api.machines.dev/v1"

// HTTP is a real Machines client. Spawn/Destroy target the Fly Machines API;
// Deploy returns ErrDeployDisabled until real deploys are switched on.
//
// Confirm the sandbox app + image and the Machines payload against your Fly org
// before relying on Spawn/Destroy in production.
type HTTP struct {
	token        string // Fly API token (org or, preferably, scoped)
	sandboxApp   string // Fly app the per-task sandbox machines run under
	sandboxImage string // OCI image containing opencode + toolchains
	client       *http.Client
}

// NewHTTP returns a real Machines client.
func NewHTTP(token, sandboxApp, sandboxImage string) *HTTP {
	return &HTTP{
		token:        token,
		sandboxApp:   sandboxApp,
		sandboxImage: sandboxImage,
		client:       &http.Client{Timeout: 60 * time.Second},
	}
}

func (h *HTTP) SpawnSandbox(ctx context.Context, taskID string) (*Sandbox, error) {
	payload := map[string]any{
		"name": "sbx-" + strings.ToLower(taskID),
		"config": map[string]any{
			"image":        h.sandboxImage,
			"guest":        map[string]any{"cpu_kind": "shared", "cpus": 2, "memory_mb": 2048},
			"auto_destroy": true,
		},
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := h.do(ctx, http.MethodPost,
		fmt.Sprintf("/apps/%s/machines", h.sandboxApp), payload, &out); err != nil {
		return nil, err
	}
	return &Sandbox{MachineID: out.ID, AppName: h.sandboxApp}, nil
}

func (h *HTTP) DestroySandbox(ctx context.Context, s *Sandbox) error {
	if s == nil {
		return nil
	}
	return h.do(ctx, http.MethodDelete,
		fmt.Sprintf("/apps/%s/machines/%s?force=true", s.AppName, s.MachineID), nil, nil)
}

// Deploy is intentionally not enabled yet — the single step left switched off.
func (h *HTTP) Deploy(_ context.Context, _ *Sandbox, _ string) (string, error) {
	return "", ErrDeployDisabled
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
