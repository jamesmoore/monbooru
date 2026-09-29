package web

import (
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/monbooru/monbooru/internal/fsx"
	"github.com/monbooru/monbooru/internal/logx"
)

// Kept out of the config: a timestamp rewritten nightly would show in
// every diff of the operator's file.
type scheduleState struct {
	LastRun time.Time `toml:"last_scheduled_run"`
}

func (s *Server) scheduleStatePath() string {
	return filepath.Join(filepath.Dir(s.configPath), "state.toml")
}

func (s *Server) lastScheduledRun() time.Time {
	var st scheduleState
	if _, err := toml.DecodeFile(s.scheduleStatePath(), &st); err != nil {
		if !os.IsNotExist(err) {
			logx.Warnf("schedule state: %v", err)
		}
		return time.Time{}
	}
	return st.LastRun
}

func (s *Server) saveScheduledRun(at time.Time) {
	path := s.scheduleStatePath()
	err := fsx.WriteAtomic(path, ".state.toml.*", func(f *os.File) error {
		return toml.NewEncoder(f).Encode(scheduleState{LastRun: at.UTC()})
	})
	if err != nil {
		logx.Warnf("schedule state: %v", err)
	}
}
