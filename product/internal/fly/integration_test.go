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

	m := NewHTTP(Options{Token: token, Org: getenv("FLY_ORG", "transcend-software"), SandboxApp: app, SandboxImage: image})
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

// TestIntegration_EnsureApp verifies per-customer app creation is real and
// idempotent. Clean up the app afterward with `fly apps destroy`.
func TestIntegration_EnsureApp(t *testing.T) {
	token := os.Getenv("FLY_API_TOKEN")
	if os.Getenv("FLY_SMOKE") == "" || token == "" {
		t.Skip("set FLY_SMOKE=1 and FLY_API_TOKEN to run the live app-create test")
	}
	m := NewHTTP(Options{Token: token, Org: getenv("FLY_ORG", "transcend-software")})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	app := getenv("FLY_TEST_APP", "transcend-forge-ea-smoke")
	if err := m.EnsureApp(ctx, app); err != nil {
		t.Fatalf("ensure app (create): %v", err)
	}
	if err := m.EnsureApp(ctx, app); err != nil {
		t.Fatalf("ensure app (idempotent): %v", err)
	}
	t.Logf("app %q ensured (create + idempotent)", app)
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
