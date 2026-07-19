// Package storage is object storage for customer-uploaded assets (photos, logos,
// content). The S3 implementation (s3.go) targets any S3-compatible backend —
// MinIO for local dev, Fly Tigris in production. A Memory implementation keeps
// the zero-config dev mode runnable without any storage backend.
//
// Only the orchestrator holds storage credentials. The sandbox never does: at
// build time the orchestrator hands the agent short-lived presigned GET URLs.
package storage

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"
)

// Store is object storage for project assets and workspace snapshots.
type Store interface {
	// Health verifies the configured object store for readiness checks.
	Health(ctx context.Context) error
	// Put stores an object under key with the given content type.
	Put(ctx context.Context, key, contentType string, r io.Reader, size int64) error
	// Get returns the object's bytes (e.g. reading a workspace snapshot tarball).
	Get(ctx context.Context, key string) ([]byte, error)
	// PresignGet returns a short-lived, read-only URL for the object.
	PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error)
	// PresignPut returns a short-lived, write-only URL for the object — how the
	// sandbox uploads its workspace snapshot without holding storage credentials.
	PresignPut(ctx context.Context, key string, expiry time.Duration) (string, error)
	// DeletePrefix permanently removes every object whose key begins with prefix.
	// Project erasure uses one namespaced prefix, so metadata is never deleted
	// while customer uploads, screenshots or snapshots remain behind.
	DeletePrefix(ctx context.Context, prefix string) error
}

// Memory is an in-process Store for the zero-config dev mode. Presigned URLs are
// placeholders — the dev/fake builder never fetches assets.
type Memory struct {
	mu      sync.Mutex
	objects map[string][]byte
}

// NewMemory returns an empty in-memory object store.
func NewMemory() *Memory { return &Memory{objects: make(map[string][]byte)} }

func (m *Memory) Health(context.Context) error { return nil }

func (m *Memory) Put(_ context.Context, key, _ string, r io.Reader, _ int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.objects[key] = b
	m.mu.Unlock()
	return nil
}

func (m *Memory) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	if !ok {
		return nil, io.EOF
	}
	return b, nil
}

func (m *Memory) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "memory://" + key, nil
}

func (m *Memory) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	return "memory://" + key, nil
}

func (m *Memory) DeletePrefix(_ context.Context, prefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.objects {
		if strings.HasPrefix(key, prefix) {
			delete(m.objects, key)
		}
	}
	return nil
}
