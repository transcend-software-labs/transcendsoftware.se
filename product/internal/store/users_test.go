package store

import (
	"context"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/pgtest"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// runUserStores runs fn against both the in-memory store and a real Postgres —
// account rules are a place the two implementations have diverged before (the
// case-sensitive UNIQUE vs. the case-insensitive lookups), so both must agree.
//
// Note: in CI the Postgres is a SHARED database reused across every test in the
// package (PG_TEST_URL), so rows persist between tests — each test here must use
// its own unique emails/ids to stay isolation-independent.
func runUserStores(t *testing.T, fn func(t *testing.T, st Store)) {
	t.Run("memory", func(t *testing.T) { fn(t, NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		pg, err := NewPostgres(context.Background(), pgtest.Start(t))
		if err != nil {
			t.Fatalf("postgres: %v", err)
		}
		fn(t, pg)
	})
}

// VerifyAndClearPassword must neutralise a pre-hijack: an unverified account
// loses its password (set by someone who never proved they own the address) and
// becomes verified, but a verified account is left untouched.
func TestVerifyAndClearPassword(t *testing.T) {
	runUserStores(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		if err := st.CreateUser(ctx, &user.User{
			ID: "vcp-attacker", Email: "vcp-victim@utest.example", PasswordHash: "attacker-hash",
			Verified: false, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create unverified: %v", err)
		}
		if err := st.CreateUser(ctx, &user.User{
			ID: "vcp-legit", Email: "vcp-real@utest.example", PasswordHash: "real-hash",
			Verified: true, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create verified: %v", err)
		}

		// A social/magic login adopts the unverified address (case-insensitively).
		if err := st.VerifyAndClearPassword(ctx, "VCP-Victim@utest.example"); err != nil {
			t.Fatalf("neutralise: %v", err)
		}
		got, _ := st.UserByEmail(ctx, "vcp-victim@utest.example")
		if !got.Verified || got.PasswordHash != "" {
			t.Errorf("unverified account must be verified with password cleared, got verified=%v hash=%q",
				got.Verified, got.PasswordHash)
		}

		// A verified password user who later uses a magic link keeps their password.
		if err := st.VerifyAndClearPassword(ctx, "vcp-real@utest.example"); err != nil {
			t.Fatalf("neutralise verified: %v", err)
		}
		if got, _ := st.UserByEmail(ctx, "vcp-real@utest.example"); got.PasswordHash != "real-hash" {
			t.Errorf("verified account's password must be preserved, got %q", got.PasswordHash)
		}
	})
}

// The users table's case-sensitive UNIQUE(email) once let "Victim@x" coexist
// with "victim@x". The lower(email) unique index (migration 0033) closes it:
// Postgres must reject the case-variant duplicate. (Memory already dedups
// case-insensitively.)
func TestCreateUser_RejectsCaseVariantDuplicate(t *testing.T) {
	runUserStores(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		if err := st.CreateUser(ctx, &user.User{
			ID: "dup-a", Email: "dup-victim@utest.example", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("first create: %v", err)
		}
		err := st.CreateUser(ctx, &user.User{
			ID: "dup-b", Email: "Dup-Victim@utest.example", CreatedAt: time.Now().UTC(),
		})
		if err != ErrEmailTaken {
			t.Errorf("case-variant duplicate must be rejected as ErrEmailTaken, got %v", err)
		}
	})
}

func TestMarkUserApproved_PersistsFirstTimestamp(t *testing.T) {
	runUserStores(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Microsecond)
		u := &user.User{ID: "approve-user", Email: "approve-user@utest.example", Verified: true, CreatedAt: now}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		if got, _ := st.UserByID(ctx, u.ID); got.Approved() {
			t.Fatal("new user should require approval")
		}
		if err := st.MarkUserApproved(ctx, u.ID, now); err != nil {
			t.Fatal(err)
		}
		later := now.Add(time.Hour)
		if err := st.MarkUserApproved(ctx, u.ID, later); err != nil {
			t.Fatal(err)
		}
		got, err := st.UserByID(ctx, u.ID)
		if err != nil || got.ApprovedAt == nil || !got.ApprovedAt.Equal(now) {
			t.Fatalf("approval timestamp = %v, want %v (err %v)", got.ApprovedAt, now, err)
		}
	})
}
