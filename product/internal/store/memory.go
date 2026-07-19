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
	mu          sync.RWMutex
	users       map[string]*user.User
	sessions    map[string]*user.Session
	loginTokens map[string]*user.LoginToken
	projects    map[string]*project.Project
	iterations  map[string]*project.Iteration
	assets      map[string]*project.Asset
	withdrawals map[string]WithdrawalRequest
	marketing   map[marketingDailyKey]int
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		users:       make(map[string]*user.User),
		sessions:    make(map[string]*user.Session),
		loginTokens: make(map[string]*user.LoginToken),
		projects:    make(map[string]*project.Project),
		iterations:  make(map[string]*project.Iteration),
		assets:      make(map[string]*project.Asset),
		withdrawals: make(map[string]WithdrawalRequest),
		marketing:   make(map[marketingDailyKey]int),
	}
}

func (m *Memory) CreateLoginToken(_ context.Context, t *user.LoginToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *t
	m.loginTokens[t.TokenHash] = &cp
	return nil
}

func (m *Memory) LoginTokenByHash(_ context.Context, tokenHash string) (*user.LoginToken, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.loginTokens[tokenHash]
	if !ok {
		return nil, project.ErrNotFound
	}
	cp := *t
	return &cp, nil
}

func (m *Memory) DeleteLoginToken(_ context.Context, tokenHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.loginTokens, tokenHash)
	return nil
}

func (m *Memory) ConsumeLoginToken(_ context.Context, tokenHash string, now time.Time) (*user.LoginToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.loginTokens[tokenHash]
	if !ok || !now.Before(t.ExpiresAt) {
		delete(m.loginTokens, tokenHash)
		return nil, project.ErrNotFound
	}
	cp := *t
	delete(m.loginTokens, tokenHash)
	return &cp, nil
}

func (m *Memory) Close() error { return nil }

func (m *Memory) Health(context.Context) error { return nil }

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

func (m *Memory) DeleteUser(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return project.ErrNotFound
	}
	delete(m.users, id)
	for key, session := range m.sessions {
		if session.UserID == id {
			delete(m.sessions, key)
		}
	}
	for key, token := range m.loginTokens {
		if strings.EqualFold(token.Email, u.Email) {
			delete(m.loginTokens, key)
		}
	}
	for projectID, pr := range m.projects {
		if pr.UserID != id {
			continue
		}
		delete(m.projects, projectID)
		for key, it := range m.iterations {
			if it.ProjectID == projectID {
				delete(m.iterations, key)
			}
		}
		for key, asset := range m.assets {
			if asset.ProjectID == projectID {
				delete(m.assets, key)
			}
		}
	}
	return nil
}

func (m *Memory) MarkUserVerified(_ context.Context, email string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if strings.EqualFold(u.Email, email) {
			u.Verified = true
		}
	}
	return nil
}

func (m *Memory) MarkUserApproved(_ context.Context, id string, approvedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return project.ErrNotFound
	}
	if u.ApprovedAt == nil {
		t := approvedAt
		u.ApprovedAt = &t
	}
	return nil
}

func (m *Memory) VerifyAndClearPassword(_ context.Context, email string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if strings.EqualFold(u.Email, email) && !u.Verified {
			u.Verified = true
			u.PasswordHash = ""
		}
	}
	return nil
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
	if p.Status == project.StatusPendingAccessApproval {
		for _, existing := range m.projects {
			if existing.UserID == p.UserID && existing.Status == project.StatusPendingAccessApproval {
				return ErrAccessApprovalPending
			}
		}
	}
	p.Version = 1
	m.projects[p.ID] = cloneProject(p)
	return nil
}

func (m *Memory) UpdateProject(_ context.Context, p *project.Project) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.projects[p.ID]
	if !ok {
		return project.ErrNotFound
	}
	if p.Version != cur.Version {
		return ErrConflict
	}
	p.Version++
	m.projects[p.ID] = cloneProject(p)
	return nil
}

func (m *Memory) DeleteProject(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[id]; !ok {
		return project.ErrNotFound
	}
	delete(m.projects, id)
	for k, it := range m.iterations {
		if it.ProjectID == id {
			delete(m.iterations, k)
		}
	}
	for k, a := range m.assets {
		if a.ProjectID == id {
			delete(m.assets, k)
		}
	}
	return nil
}

func (m *Memory) ProjectByID(_ context.Context, id string) (*project.Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.projects[id]
	if !ok {
		return nil, project.ErrNotFound
	}
	return cloneProject(p), nil
}

func (m *Memory) ProjectByPreviewHost(_ context.Context, host string) (*project.Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if host == "" { // '' means "never assigned" — must not match anything
		return nil, project.ErrNotFound
	}
	for _, p := range m.projects { // scan is fine at memory-store scale
		if p.PreviewHost == host {
			return cloneProject(p), nil
		}
	}
	return nil, project.ErrNotFound
}

func (m *Memory) ProjectsByUser(_ context.Context, userID string) ([]*project.Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*project.Project
	for _, p := range m.projects {
		if p.UserID == userID {
			out = append(out, cloneProject(p))
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
		out = append(out, cloneProject(p))
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
			out = append(out, cloneProject(p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) PendingDomainProjects(_ context.Context) ([]*project.Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*project.Project
	for _, p := range m.projects {
		switch p.DomainStatus {
		case project.DomainRegistering, project.DomainPendingDNS, project.DomainVerifying:
			out = append(out, cloneProject(p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func cloneSlice[T any](in []T) []T {
	if in == nil {
		return nil
	}
	out := make([]T, len(in))
	copy(out, in)
	return out
}

func cloneStrings(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// cloneProject keeps the in-memory store's lock boundary honest. Project owns
// several maps and nested slices; a shallow copy would let a caller mutate the
// stored row before UpdateProject performs its optimistic-version check.
func cloneProject(in *project.Project) *project.Project {
	out := *in
	out.Questions = cloneSlice(in.Questions)
	out.DesignOptions = cloneSlice(in.DesignOptions)
	for i := range out.DesignOptions {
		out.DesignOptions[i].Palette = cloneSlice(in.DesignOptions[i].Palette)
		out.DesignOptions[i].HeroConcepts = cloneSlice(in.DesignOptions[i].HeroConcepts)
		for j := range out.DesignOptions[i].HeroConcepts {
			out.DesignOptions[i].HeroConcepts[j].Palette = cloneSlice(in.DesignOptions[i].HeroConcepts[j].Palette)
		}
	}
	out.Screenshots = cloneSlice(in.Screenshots)
	out.Findings = cloneSlice(in.Findings) // nil vs empty is semantically significant
	out.DomainRecords = cloneSlice(in.DomainRecords)
	out.Spec.Pages = cloneSlice(in.Spec.Pages)
	for i := range out.Spec.Pages {
		out.Spec.Pages[i].Paths = cloneSlice(in.Spec.Pages[i].Paths)
		out.Spec.Pages[i].Names = cloneStrings(in.Spec.Pages[i].Names)
	}
	out.Spec.NotIncluded = cloneSlice(in.Spec.NotIncluded)
	out.Spec.ContentNeeded = cloneSlice(in.Spec.ContentNeeded)
	for i := range out.Spec.ContentNeeded {
		out.Spec.ContentNeeded[i].Names = cloneStrings(in.Spec.ContentNeeded[i].Names)
	}
	out.ContentAnswers = cloneStrings(in.ContentAnswers)
	if in.ContentRosters != nil {
		out.ContentRosters = make(map[string][]project.RosterEntry, len(in.ContentRosters))
		for slot, entries := range in.ContentRosters {
			out.ContentRosters[slot] = cloneSlice(entries)
		}
	}
	if in.PendingImages != nil {
		out.PendingImages = make(map[string]project.ImageCandidates, len(in.PendingImages))
		for slot, candidates := range in.PendingImages {
			candidates.Keys = cloneSlice(candidates.Keys)
			out.PendingImages[slot] = candidates
		}
	}
	return &out
}

func (m *Memory) CreateIteration(_ context.Context, it *project.Iteration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *it
	m.iterations[it.ID] = &cp
	return nil
}

func (m *Memory) ReserveIteration(_ context.Context, it *project.Iteration, maxConcurrent, maxDaily int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	active, recent := 0, 0
	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	for _, existing := range m.iterations {
		if existing.Status == project.StatusBuilding {
			active++
		}
		if !existing.CreatedAt.Before(cutoff) {
			recent++
		}
	}
	if maxConcurrent > 0 && active >= maxConcurrent {
		return ErrBuildCapacity
	}
	if maxDaily > 0 && recent >= maxDaily {
		return ErrBuildDailyCap
	}
	cp := *it
	if cp.HeartbeatAt.IsZero() {
		cp.HeartbeatAt = cp.CreatedAt
	}
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

func (m *Memory) SetAssetDescription(_ context.Context, assetID, description string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.assets[assetID]; ok {
		a.Description = description
	}
	return nil
}

func (m *Memory) RecordWithdrawalRequest(_ context.Context, request WithdrawalRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.withdrawals[request.ID] = request
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

func (m *Memory) IterationsSince(_ context.Context, t time.Time) ([]*project.Iteration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*project.Iteration
	for _, it := range m.iterations {
		if !it.CreatedAt.Before(t) {
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
