//go:build !tagger

package tagger

import (
	"context"
	"errors"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/jobs"
)

func CheckProviderAvailable(_ string) error {
	return errors.New("auto-tagger disabled (built without -tags tagger)")
}

func IsAvailable(_ *config.Config) bool { return false }

func buildSupportsInference() bool { return false }

func UnavailableReason(_ *config.Config) string {
	return "inference disabled (built without -tags tagger)"
}

func AvailableTaggers(cfg *config.Config) []TaggerStatus {
	list := DiscoverTaggers(cfg)
	for i := range list {
		list[i].Available = false
		list[i].Reason = "inference disabled (built without -tags tagger)"
	}
	return list
}

func RunWithTaggers(_ context.Context, _ *db.DB, _ *config.Config, _ []int64, _ []TaggerStatus, _ *jobs.Manager, _ string, _ string) (int, error) {
	return 0, nil
}

func ReleaseIdle(_ time.Duration) bool { return false }

func ReleaseAll() {}

func Status() CacheStatus { return CacheStatus{} }
