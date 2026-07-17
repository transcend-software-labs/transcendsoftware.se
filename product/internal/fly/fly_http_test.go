package fly

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// The sweep's ownership rules, exercised against the real HTTP client:
// young machines and active builds' machines survive, orphans older than the
// grace die, and even an owned machine dies past the hard backstop.
func TestSweepSandboxes_OwnershipRules(t *testing.T) {
	now := time.Now()
	machines := []map[string]any{
		{"id": "young-orphan", "created_at": now.Add(-5 * time.Minute)},
		{"id": "old-orphan", "created_at": now.Add(-30 * time.Minute)},
		{"id": "active-build", "created_at": now.Add(-60 * time.Minute)},
		{"id": "ancient-owned", "created_at": now.Add(-3 * time.Hour)},
	}
	var mu sync.Mutex
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apps/sbx-app/machines":
			_ = json.NewEncoder(w).Encode(machines)
		case r.Method == http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	h := NewHTTP(Options{Token: "t", SandboxApp: "sbx-app", MachinesURL: srv.URL})
	n, err := h.SweepSandboxes(t.Context(), 15*time.Minute, 150*time.Minute,
		[]string{"active-build", "ancient-owned"})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Fatalf("reaped %d machines, want 2 (old orphan + ancient owned)", n)
	}
	want := map[string]bool{
		fmt.Sprintf("/apps/sbx-app/machines/%s", "old-orphan"):    true,
		fmt.Sprintf("/apps/sbx-app/machines/%s", "ancient-owned"): true,
	}
	for _, p := range deleted {
		if !want[p] {
			t.Errorf("unexpected delete %s", p)
		}
		delete(want, p)
	}
	for p := range want {
		t.Errorf("expected delete %s to happen", p)
	}
}
