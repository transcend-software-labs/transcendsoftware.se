package storage

import (
	"bytes"
	"context"
	"testing"
)

func TestMemoryDeletePrefix(t *testing.T) {
	st := NewMemory()
	ctx := context.Background()
	for _, key := range []string{"projects/a/assets/1", "projects/a/snapshot.tgz", "projects/b/assets/1"} {
		if err := st.Put(ctx, key, "text/plain", bytes.NewBufferString(key), int64(len(key))); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DeletePrefix(ctx, "projects/a/"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, "projects/a/assets/1"); err == nil {
		t.Fatal("matching object remained")
	}
	if _, err := st.Get(ctx, "projects/b/assets/1"); err != nil {
		t.Fatal("unrelated object was deleted")
	}
}
