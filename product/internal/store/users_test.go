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
		u := &user.User{ID: "approve-user", Email: "approve-user@utest.example", CreatedAt: now}
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
