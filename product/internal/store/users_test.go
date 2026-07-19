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
			ID: "attacker", Email: "victim@example.com", PasswordHash: "attacker-hash",
			Verified: false, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create unverified: %v", err)
		}
		if err := st.CreateUser(ctx, &user.User{
			ID: "legit", Email: "real@example.com", PasswordHash: "real-hash",
			Verified: true, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create verified: %v", err)
		}

		// A social/magic login adopts the unverified address (case-insensitively).
		if err := st.VerifyAndClearPassword(ctx, "Victim@example.com"); err != nil {
			t.Fatalf("neutralise: %v", err)
		}
		got, _ := st.UserByEmail(ctx, "victim@example.com")
		if !got.Verified || got.PasswordHash != "" {
			t.Errorf("unverified account must be verified with password cleared, got verified=%v hash=%q",
				got.Verified, got.PasswordHash)
		}

		// A verified password user who later uses a magic link keeps their password.
		if err := st.VerifyAndClearPassword(ctx, "real@example.com"); err != nil {
			t.Fatalf("neutralise verified: %v", err)
		}
		if got, _ := st.UserByEmail(ctx, "real@example.com"); got.PasswordHash != "real-hash" {
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
			ID: "a", Email: "victim@example.com", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("first create: %v", err)
		}
		err := st.CreateUser(ctx, &user.User{
			ID: "b", Email: "Victim@example.com", CreatedAt: time.Now().UTC(),
		})
		if err != ErrEmailTaken {
			t.Errorf("case-variant duplicate must be rejected as ErrEmailTaken, got %v", err)
		}
	})
}
