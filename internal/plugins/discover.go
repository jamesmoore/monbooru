package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"

	"github.com/monbooru/monbooru/internal/shared/logx"
)

// Contract moves on any change a plugin built against the old one would
// trip over: MONBOORU_URL, the stdin-close stop, the pair offer and its
// buttons, the /health and /api/v1/pair/remove calls back, the bearer
// tokens, or the relay call and the mount.
const Contract = 1

// Contract is the plugin contract the folder was built for; 0 reads as 1.
type Manifest struct {
	Command        string   `toml:"command"`
	Args           []string `toml:"args"`
	CommandWindows string   `toml:"command_windows"`
	ArgsWindows    []string `toml:"args_windows"`
	Contract       int      `toml:"contract"`
}

func (m Manifest) launchFor(goos string) (string, []string) {
	if goos == "windows" && m.CommandWindows != "" {
		if m.ArgsWindows != nil {
			return m.CommandWindows, m.ArgsWindows
		}
		return m.CommandWindows, m.Args
	}
	return m.Command, m.Args
}

type Refusal struct {
	Name   string
	Reason string
}

var (
	warnedMu sync.Mutex
	warned   = map[string]map[string]string{}
)

// Discovery runs nothing: starting a plugin is the operator's choice.
func Discover(dir string, skip func(name string) (string, bool)) ([]Launch, []Refusal) {
	if dir == "" {
		return nil, nil
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	warnedMu.Lock()
	defer warnedMu.Unlock()
	last, reasons := warned[dir], map[string]string{}
	var out []Launch
	var refused []Refusal
	for _, it := range items {
		name := it.Name()
		folder := filepath.Join(dir, name)
		if !it.IsDir() && (it.Type()&os.ModeSymlink == 0 || !isDir(folder)) {
			continue
		}
		launch, reason, ok := inspect(folder, name, skip)
		if !ok {
			continue
		}
		if reason != "" {
			if last[name] != reason {
				logx.Warnf("plugins: skipping %s: %s", folder, reason)
			}
			reasons[name] = reason
			refused = append(refused, Refusal{Name: name, Reason: reason})
			continue
		}
		out = append(out, launch)
	}
	warned[dir] = reasons
	return out, refused
}

func inspect(folder, name string, skip func(name string) (string, bool)) (Launch, string, bool) {
	var m Manifest
	if _, err := toml.DecodeFile(filepath.Join(folder, "plugin.toml"), &m); err != nil {
		if os.IsNotExist(err) {
			return Launch{}, "", false
		}
		return Launch{}, err.Error(), true
	}
	if m.Contract > Contract {
		return Launch{}, fmt.Sprintf("it needs plugin contract %d, this monbooru speaks %d", m.Contract, Contract), true
	}
	if skip != nil {
		if reason, refused := skip(name); refused {
			return Launch{}, reason, true
		}
	}
	command, args := m.launchFor(runtime.GOOS)
	if command == "" {
		return Launch{}, "plugin.toml names no command", true
	}
	return Launch{
		Name:    name,
		Command: resolveCommand(folder, command),
		Args:    args,
		Dir:     folder,
	}, "", true
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func resolveCommand(dir, command string) string {
	if filepath.IsAbs(command) || !strings.ContainsAny(command, `/\`) {
		return command
	}
	return filepath.Join(dir, command)
}
