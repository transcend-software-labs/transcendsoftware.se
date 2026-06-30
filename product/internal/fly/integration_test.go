package fly

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestIntegration_SpawnDestroy exercises the real Fly Machines API: it spawns a
// sandbox from the real image, asserts it came back started with a private
// address, then destroys it. Skipped unless explicitly enabled:
//
//	FLY_SMOKE=1 FLY_API_TOKEN=$(fly auth token) \
//	  go test ./internal/fly/ -run Integration -v
func TestIntegration_SpawnDestroy(t *testing.T) {
	token := os.Getenv("FLY_API_TOKEN")
	if os.Getenv("FLY_SMOKE") == "" || token == "" {
		t.Skip("set FLY_SMOKE=1 and FLY_API_TOKEN to run the live Fly spawn/destroy test")
	}
	app := getenv("FLY_SANDBOX_APP", "transcend-forge-sandbox")
	image := getenv("FLY_SANDBOX_IMAGE", "registry.fly.io/transcend-forge-sandbox:20260630")

	m := NewHTTP(token, app, image)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sb, err := m.SpawnSandbox(ctx, SpawnSpec{
		TaskID: "smoke",
		Env:    map[string]string{"FORGE_SMOKE": "1"},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Logf("spawned machine=%s addr=%s", sb.MachineID, sb.Addr)

	if sb.MachineID == "" {
		t.Error("expected a machine id")
	}
	if sb.Addr == "" {
		t.Error("expected a private address (private_ip parsing / wait failed)")
	}

	if err := m.DestroySandbox(ctx, sb); err != nil {
		t.Errorf("destroy: %v", err)
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
