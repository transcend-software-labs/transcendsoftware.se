package builder

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
	"github.com/transcend-software-labs/rasmus-ai/internal/storage"
)

// TestIntegration_SnapshotRoundTrip exercises the real snapshot save/restore
// path — the builder's exec commands against a live Fly sandbox and real
// presigned Tigris URLs — which unit tests (fakes) can't cover.
//
// NOTE: SpawnSandbox waits for opencode over Fly's private 6PN network, so this
// must run from inside that network (on Fly, or via `fly wireguard`); from a
// laptop the readiness wait fails with "no route to host". The exec + Tigris
// round trip itself uses only public endpoints.
//
// Skipped unless enabled:
//
//	SNAPSHOT_SMOKE=1 FLY_API_TOKEN=... FLY_ORG=... FLY_SANDBOX_APP=... \
//	  FLY_SANDBOX_IMAGE=... STORAGE_ENDPOINT=... STORAGE_ACCESS_KEY=... \
//	  STORAGE_SECRET_KEY=... STORAGE_BUCKET=... \
//	  go test ./internal/builder/ -run Integration_Snapshot -v
func TestIntegration_SnapshotRoundTrip(t *testing.T) {
	if os.Getenv("SNAPSHOT_SMOKE") == "" {
		t.Skip("set SNAPSHOT_SMOKE=1 (+ FLY_* and STORAGE_* env) to run the live snapshot test")
	}
	machines := fly.NewHTTP(fly.Options{
		Token:        os.Getenv("FLY_API_TOKEN"),
		Org:          os.Getenv("FLY_ORG"),
		SandboxApp:   os.Getenv("FLY_SANDBOX_APP"),
		SandboxImage: os.Getenv("FLY_SANDBOX_IMAGE"),
	})
	store, err := storage.NewS3(storage.NewS3Params{
		Endpoint:  os.Getenv("STORAGE_ENDPOINT"),
		AccessKey: os.Getenv("STORAGE_ACCESS_KEY"),
		SecretKey: os.Getenv("STORAGE_SECRET_KEY"),
		Bucket:    os.Getenv("STORAGE_BUCKET"),
		Region:    "auto",
		UseSSL:    true,
	})
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	b := NewSandbox(machines, func(string) opencode.Driver { return opencode.NewFake() }, Config{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sb, err := machines.SpawnSandbox(ctx, fly.SpawnSpec{TaskID: "snaptest"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer func() { _ = machines.DestroySandbox(context.WithoutCancel(ctx), sb) }()

	// Write a marker into the workspace.
	const marker = "SNAPSHOT_ROUNDTRIP_OK"
	if r, err := machines.Exec(ctx, sb.MachineID,
		[]string{"/bin/sh", "-c", "mkdir -p /workspace && echo " + marker + " > /workspace/marker.txt"}, 30); err != nil || r.ExitCode != 0 {
		t.Fatalf("seed workspace: err=%v exit=%d %s", err, r.ExitCode, r.Stderr)
	}

	key := "snapshots/roundtrip-test.tgz"
	putURL, err := store.PresignPut(ctx, key, 10*time.Minute)
	if err != nil {
		t.Fatalf("presign put: %v", err)
	}
	if err := b.saveSnapshot(ctx, sb.MachineID, putURL); err != nil {
		t.Fatalf("saveSnapshot: %v", err)
	}

	// Wipe the workspace, then restore from the snapshot.
	if r, err := machines.Exec(ctx, sb.MachineID,
		[]string{"/bin/sh", "-c", "rm -rf /workspace/* && mkdir -p /workspace"}, 30); err != nil || r.ExitCode != 0 {
		t.Fatalf("wipe workspace: err=%v exit=%d %s", err, r.ExitCode, r.Stderr)
	}
	getURL, err := store.PresignGet(ctx, key, 10*time.Minute)
	if err != nil {
		t.Fatalf("presign get: %v", err)
	}
	if err := b.restoreSnapshot(ctx, sb.MachineID, getURL); err != nil {
		t.Fatalf("restoreSnapshot: %v", err)
	}

	// The marker must be back.
	r, err := machines.Exec(ctx, sb.MachineID, []string{"/bin/sh", "-c", "cat /workspace/marker.txt"}, 30)
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("read marker: err=%v exit=%d %s", err, r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stdout, marker) {
		t.Fatalf("marker not restored; got %q", r.Stdout)
	}
	t.Logf("snapshot round-trip OK: %q", strings.TrimSpace(r.Stdout))
}
