package monitor

import (
	"context"

	"github.com/daschmidt1994/borg-backup-monitor/internal/borg"
	"github.com/daschmidt1994/borg-backup-monitor/internal/config"
	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
)

type fakeBackend struct{}

func (fakeBackend) Detect(context.Context) *borg.Info {
	return &borg.Info{Supported: true, Version: "1.4.0"}
}
func (fakeBackend) List(_ context.Context, r *config.Repository) (*store.RepoSnapshot, *borg.Error) {
	return &store.RepoSnapshot{RepoID: r.ID, OK: true}, nil
}
func (fakeBackend) Sizes(context.Context, *config.Repository) (*store.Sizes, *borg.Error) {
	return nil, nil
}
func (fakeBackend) Files(context.Context, *config.Repository, string, string, int) ([]borg.Item, bool, *borg.Error) {
	return nil, false, nil
}
func (fakeBackend) Extract(context.Context, *config.Repository, borg.ExtractSpec) (string, int, error) {
	return "", 0, nil
}
func (fakeBackend) Check(context.Context, *config.Repository) (string, int, error) { return "", 0, nil }
