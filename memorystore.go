package synthusers

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryStore is an in-process Store, good for tests, demos and single-node
// deployments where losing history on restart is acceptable.
type MemoryStore struct {
	mu      sync.RWMutex
	targets map[string]Target
	runs    []Run
	issues  map[string]*Issue // by fingerprint
	seq     int64
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		targets: make(map[string]Target),
		issues:  make(map[string]*Issue),
	}
}

func (m *MemoryStore) UpsertTarget(_ context.Context, t Target) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	m.targets[t.ID] = t
	return nil
}

func (m *MemoryStore) DeleteTarget(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.targets, id)
	return nil
}

func (m *MemoryStore) ListTargets(_ context.Context) ([]Target, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Target, 0, len(m.targets))
	for _, t := range m.targets {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemoryStore) SaveRun(_ context.Context, r *Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	r.ID = m.seq
	m.runs = append(m.runs, *r)
	if len(m.runs) > 10000 { // bound memory usage; newest runs matter most
		m.runs = m.runs[len(m.runs)-10000:]
	}
	return nil
}

func (m *MemoryStore) ListRuns(_ context.Context, targetID string, limit int) ([]Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if limit <= 0 {
		limit = 50
	}
	out := make([]Run, 0, limit)
	for i := len(m.runs) - 1; i >= 0 && len(out) < limit; i-- {
		if targetID == "" || m.runs[i].TargetID == targetID {
			out = append(out, m.runs[i])
		}
	}
	return out, nil
}

func (m *MemoryStore) ReportFinding(_ context.Context, targetID string, f Finding) (bool, error) {
	fp := FingerprintOf(targetID, f)
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.issues[fp]; ok {
		existing.Summary = f.Summary
		existing.Detail = f.Detail
		existing.Severity = f.Severity
		existing.URL = f.URL
		existing.LastSeen = now
		existing.Occurrences++
		if existing.Status == StatusResolved {
			existing.Status = StatusOpen
			existing.ResolvedAt = nil
		}
		return false, nil
	}
	m.issues[fp] = &Issue{
		Fingerprint: fp,
		TargetID:    targetID,
		Type:        f.Type,
		Severity:    f.Severity,
		URL:         f.URL,
		URLKey:      f.URLKey,
		Summary:     f.Summary,
		Detail:      f.Detail,
		Status:      StatusOpen,
		FirstSeen:   now,
		LastSeen:    now,
		Occurrences: 1,
	}
	return true, nil
}

func (m *MemoryStore) ResolveIssues(_ context.Context, fingerprints []string) (int, error) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, fp := range fingerprints {
		if iss, ok := m.issues[fp]; ok && iss.Status == StatusOpen {
			iss.Status = StatusResolved
			iss.ResolvedAt = &now
			n++
		}
	}
	return n, nil
}

func (m *MemoryStore) ListIssues(_ context.Context, f IssueFilter) ([]Issue, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	out := make([]Issue, 0, limit)
	for _, iss := range m.issues {
		if f.Status != "" && iss.Status != f.Status {
			continue
		}
		if f.Severity != "" && string(iss.Severity) != f.Severity {
			continue
		}
		if f.TargetID != "" && iss.TargetID != f.TargetID {
			continue
		}
		if f.Type != "" && iss.Type != f.Type {
			continue
		}
		out = append(out, *iss)
	}
	// open first, then by severity, then most recently seen
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Status == StatusOpen) != (b.Status == StatusOpen) {
			return a.Status == StatusOpen
		}
		if ra, rb := SeverityRank(a.Severity), SeverityRank(b.Severity); ra != rb {
			return ra < rb
		}
		return a.LastSeen.After(b.LastSeen)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) Stats(_ context.Context) (Stats, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := Stats{
		Targets:    len(m.targets),
		BySeverity: map[string]int{},
		ByType:     map[string]int{},
	}
	dayAgo := time.Now().Add(-24 * time.Hour)
	for _, iss := range m.issues {
		if iss.Status != StatusOpen {
			continue
		}
		s.OpenIssues++
		s.BySeverity[string(iss.Severity)]++
		s.ByType[iss.Type]++
	}
	for _, r := range m.runs {
		if r.StartedAt.After(dayAgo) {
			s.RunsLast24h++
			s.PagesLast24h += r.Pages
		}
	}
	return s, nil
}

func (m *MemoryStore) Close() error { return nil }
