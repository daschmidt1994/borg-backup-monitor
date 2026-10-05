package borg

import (
	"context"

	"github.com/daschmidt1994/borg-backup-monitor/internal/config"
	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

// Backend is what the monitor needs from borg. The real one is *Runner;
// the demo mode has a simulated one.
type Backend interface {
	Detect(ctx context.Context) *Info
	List(ctx context.Context, repo *config.Repository) (*store.RepoSnapshot, *Error)
	Sizes(ctx context.Context, repo *config.Repository) (*store.Sizes, *Error)
	Files(ctx context.Context, repo *config.Repository, archive, prefix string, max int) ([]Item, bool, *Error)
	Extract(ctx context.Context, repo *config.Repository, spec ExtractSpec) (stderr string, code int, err error)
}

var _ Backend = (*Runner)(nil)
