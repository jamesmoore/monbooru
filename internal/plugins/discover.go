package plugins

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/monbooru/monbooru/internal/logx"
)

type Manifest struct {
	Command        string   `toml:"command"`
	Args           []string `toml:"args"`
	CommandWindows string   `toml:"command_windows"`
	ArgsWindows    []string `toml:"args_windows"`
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

// Discovery runs nothing: starting a plugin is the operator's choice.
func Discover(dir string, skip func(name string) (string, bool)) []Launch {
	if dir == "" {
		return nil
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Launch
	for _, it := range items {
		if !it.IsDir() {
			continue
		}
		name := it.Name()
		folder := filepath.Join(dir, name)
		var m Manifest
		if _, err := toml.DecodeFile(filepath.Join(folder, "plugin.toml"), &m); err != nil {
			if !os.IsNotExist(err) {
				logx.Warnf("plugins: skipping %s: %v", folder, err)
			}
			continue
		}
		if skip != nil {
			if reason, refused := skip(name); refused {
				logx.Warnf("plugins: skipping %s: %s", folder, reason)
				continue
			}
		}
		command, args := m.launchFor(runtime.GOOS)
		if command == "" {
			logx.Warnf("plugins: skipping %s: plugin.toml names no command", folder)
			continue
		}
		out = append(out, Launch{
			Name:    name,
			Command: resolveCommand(folder, command),
			Args:    args,
			Dir:     folder,
		})
	}
	return out
}

func resolveCommand(dir, command string) string {
	if filepath.IsAbs(command) || !strings.ContainsAny(command, `/\`) {
		return command
	}
	return filepath.Join(dir, command)
}
