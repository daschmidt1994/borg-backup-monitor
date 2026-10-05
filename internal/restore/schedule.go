package restore

import (
	"context"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/daschmidt1994/borg-backup-monitor/internal/config"
	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

const AutoUser = "automatisch"

// RunScheduler starts automatic sample restores (restore_test.schedule).
func (m *Manager) RunScheduler(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.autoDue(ctx)
		}
	}
}

func (m *Manager) autoDue(ctx context.Context) {
	now := time.Now()
	for _, ref := range m.Cfg.Jobs() {
		rt := ref.Job.RestoreTest
		if rt.Schedule <= 0 || !ref.Job.RestoreEnabled(m.Cfg.Restore.Enabled) || !config.InWindow(rt.Window, now, m.Cfg.Location) {
			continue
		}
		repo := m.nextRepo(ref.Job)
		if repo == nil {
			continue
		}
		var lastAuto time.Time
		m.Store.Read(func(s *store.State) {
			for _, t := range s.RestoreTests {
				if t.JobID == ref.Job.ID && t.StartedBy == AutoUser && t.StartedAt.After(lastAuto) {
					lastAuto = t.StartedAt
				}
			}
		})
		if now.Sub(lastAuto) < rt.Schedule.D() || m.Mon.JobRunning(ref.Job.ID) {
			continue
		}
		m.mu.Lock()
		busy := m.running
		m.mu.Unlock()
		if busy {
			continue
		}
		if err := m.autoTest(ctx, ref.Job, repo); err != nil {
			m.Mon.Log.Warn("automatic restore test not started", "job", ref.Job.ID, "err", err)
		}
	}
}

// nextRepo: the queried repository of the job whose last test is oldest.
func (m *Manager) nextRepo(job *config.Job) *config.Repository {
	var best *config.Repository
	var bestAt time.Time
	for i := range job.Repositories {
		r := &job.Repositories[i]
		if !r.Queried() {
			continue
		}
		var last time.Time
		m.Store.Read(func(s *store.State) {
			for _, t := range s.RestoreTests {
				if t.RepoID == r.ID && t.StartedAt.After(last) {
					last = t.StartedAt
				}
			}
		})
		if best == nil || last.Before(bestAt) {
			best, bestAt = r, last
		}
	}
	return best
}

// autoTest picks random files of the newest archive and runs a test. If
// that is impossible (e.g. a sample path is missing), a failed test is
// recorded, so the problem shows up in the dashboard.
func (m *Manager) autoTest(ctx context.Context, job *config.Job, repo *config.Repository) error {
	var archive string
	m.Store.Read(func(s *store.State) {
		if sn := s.Repos[repo.ID]; sn != nil && sn.OK && sn.Latest != nil {
			archive = sn.Latest.Name
		}
	})
	if archive == "" {
		return nil // nothing known yet
	}
	rt := job.RestoreTest
	limit := int64(m.Cfg.Restore.MaxBytes) / int64(rt.SampleFiles)
	var candidates []string
	var problems []string
	for _, p := range rt.SamplePaths {
		items, _, berr := m.Backend.Files(ctx, repo, archive, strings.Trim(p, "/"), 2000)
		if berr != nil {
			problems = append(problems, p+": "+berr.Finding.Summary)
			continue
		}
		n := 0
		for _, it := range items {
			if it.Type == "file" && it.Size > 0 && it.Size <= limit {
				candidates = append(candidates, it.Path)
				n++
			}
		}
		if n == 0 {
			problems = append(problems, "„"+p+"“: keine geeigneten Dateien im Archiv – fehlt die Quelle?")
		}
	}
	rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	if len(candidates) > rt.SampleFiles {
		candidates = candidates[:rt.SampleFiles]
	}
	if len(candidates) == 0 {
		return m.recordFailed(job, repo, archive, problems)
	}
	p, err := m.PlanTest(ctx, repo.ID, archive, candidates)
	if err != nil {
		return m.recordFailed(job, repo, archive, append(problems, err.Error()))
	}
	if !p.OK {
		return m.recordFailed(job, repo, archive, append(problems, p.Problems...))
	}
	_, err = m.Start(p.ID, AutoUser)
	return err
}

func (m *Manager) recordFailed(job *config.Job, repo *config.Repository, archive string, problems []string) error {
	now := time.Now()
	t := &store.RestoreTest{ID: newID(), RepoID: repo.ID, JobID: job.ID, Archive: archive, Paths: job.RestoreTest.SamplePaths,
		StartedAt: now, FinishedAt: &now, StartedBy: AutoUser, State: "failed", Removed: true}
	for _, p := range problems {
		t.Findings = append(t.Findings, store.Finding{Level: "error", Summary: p})
	}
	return m.Store.Update(func(s *store.State) { s.RestoreTests = append(s.RestoreTests, t) })
}
