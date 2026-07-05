package store

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// Memory is an in-memory Store. It is the default in dev mode and keeps the
// app fully runnable without a database. All data is lost on restart.
type Memory struct {
	mu         sync.RWMutex
	users      map[string]*user.User
	sessions   map[string]*user.Session
	projects   map[string]*project.Project
	iterations map[string]*project.Iteration
	assets     map[string]*project.Asset
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		users:      make(map[string]*user.User),
		sessions:   make(map[string]*user.Session),
		projects:   make(map[string]*project.Project),
		iterations: make(map[string]*project.Iteration),
		assets:     make(map[string]*project.Asset),
	}
}

func (m *Memory) Close() error { return nil }

func (m *Memory) CreateUser(_ context.Context, u *user.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.users {
		if strings.EqualFold(existing.Email, u.Email) {
			return ErrEmailTaken
		}
	}
	cp := *u
	m.users[u.ID] = &cp
	return nil
}

func (m *Memory) UserByEmail(_ context.Context, email string) (*user.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, u := range m.users {
		if strings.EqualFold(u.Email, email) {
			cp := *u
			return &cp, nil
		}
	}
	return nil, project.ErrNotFound
}

func (m *Memory) UserByID(_ context.Context, id string) (*user.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[id]
	if !ok {
		return nil, project.ErrNotFound
	}
	cp := *u
	return &cp, nil
}

func (m *Memory) CreateSession(_ context.Context, s *user.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *s
	m.sessions[s.TokenHash] = &cp
	return nil
}

func (m *Memory) SessionByTokenHash(_ context.Context, tokenHash string) (*user.Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[tokenHash]
	if !ok {
		return nil, project.ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (m *Memory) DeleteSession(_ context.Context, tokenHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, tokenHash)
	return nil
}

func (m *Memory) DeleteExpiredSessions(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for k, s := range m.sessions {
		if now.After(s.ExpiresAt) {
			delete(m.sessions, k)
		}
	}
	return nil
}

func (m *Memory) CreateProject(_ context.Context, p *project.Project) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *p
	m.projects[p.ID] = &cp
	return nil
}

func (m *Memory) UpdateProject(_ context.Context, p *project.Project) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[p.ID]; !ok {
		return project.ErrNotFound
	}
	cp := *p
	m.projects[p.ID] = &cp
	return nil
}

func (m *Memory) ProjectByID(_ context.Context, id string) (*project.Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.projects[id]
	if !ok {
		return nil, project.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *Memory) ProjectsByUser(_ context.Context, userID string) ([]*project.Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*project.Project
	for _, p := range m.projects {
		if p.UserID == userID {
			cp := *p
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (m *Memory) Projects(_ context.Context) ([]*project.Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*project.Project, 0, len(m.projects))
	for _, p := range m.projects {
		cp := *p
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) EscalatedProjects(_ context.Context) ([]*project.Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*project.Project
	for _, p := range m.projects {
		if p.Status == project.StatusEscalated {
			cp := *p
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) CreateIteration(_ context.Context, it *project.Iteration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *it
	m.iterations[it.ID] = &cp
	return nil
}

func (m *Memory) UpdateIteration(_ context.Context, it *project.Iteration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.iterations[it.ID]; !ok {
		return project.ErrNotFound
	}
	cp := *it
	m.iterations[it.ID] = &cp
	return nil
}

func (m *Memory) CreateAsset(_ context.Context, a *project.Asset) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *a
	m.assets[a.ID] = &cp
	return nil
}

func (m *Memory) AssetsByProject(_ context.Context, projectID string) ([]*project.Asset, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*project.Asset
	for _, a := range m.assets {
		if a.ProjectID == projectID {
			cp := *a
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) ActiveIterations(_ context.Context) ([]*project.Iteration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*project.Iteration
	for _, it := range m.iterations {
		if it.Status == project.StatusBuilding {
			cp := *it
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) IterationsByProject(_ context.Context, projectID string) ([]*project.Iteration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*project.Iteration
	for _, it := range m.iterations {
		if it.ProjectID == projectID {
			cp := *it
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}
