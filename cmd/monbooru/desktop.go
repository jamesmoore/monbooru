package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/desktop"
	"github.com/monbooru/monbooru/internal/fsx"
	"github.com/monbooru/monbooru/internal/logx"
	internalweb "github.com/monbooru/monbooru/internal/web"
)

const appName = "monbooru"

// Stamped "true" at link time (-X) by the desktop artifacts.
var defaultDesktop string

// A loopback request to a process that answers at once or is not there.
const probeTimeout = 500 * time.Millisecond

// A portable install keeps the config package's relative paths: OS-native
// ones would pin the folder to the machine it was first run on.
func desktopSeed(l desktop.Layout) func(*config.Config) {
	return func(cfg *config.Config) {
		if !l.Portable {
			cfg.Paths.DataPath = l.DataDir
			cfg.Paths.ModelPath = filepath.Join(l.DataDir, "models")
			cfg.Galleries[0].GalleryPath = seedGalleryDir(l)
		}
		cfg.Schedule.Mode = config.ScheduleAtTimeCatchup
		// A desktop bug report carries only the log, which warn leaves empty.
		cfg.Log.Level = "info"
	}
}

// Only the folder the archive promises: a gallery on a disk that is not
// mounted must stay degraded.
func portableGallery(l desktop.Layout, cfg *config.Config) {
	g := cfg.FindGallery(cfg.DefaultGallery)
	if g == nil {
		return
	}
	dir := filepath.Join(l.ConfigDir, "gallery")
	if g.GalleryPath != dir {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logx.Warnf("could not create a gallery folder at %s: %v", dir, err)
	}
}

// Created, not only named: a missing gallery folder boots into degraded mode.
func seedGalleryDir(l desktop.Layout) string {
	base := desktop.PicturesDir()
	if base == "" {
		base = l.DataDir
	}
	dir := filepath.Join(base, appName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		dir = filepath.Join(l.DataDir, "gallery")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			logx.Warnf("could not create a gallery folder at %s: %v", dir, err)
		}
	}
	return dir
}

// claimPort returns a diagnostic rather than exiting: the shutdown path
// still owes the database and watchers a close.
func claimPort(addr string, openBrowser bool) (string, bool) {
	local := desktop.LoopbackAddr(addr)
	inst, found := desktop.Probe(local, probeTimeout)
	if !found {
		return "", true
	}
	if inst.App == appName {
		// stdout: a -no-browser launch from a terminal shows nothing else.
		fmt.Printf("%s is already running on http://%s\n", appName, local)
		if openBrowser {
			if err := desktop.OpenBrowser("http://" + local); err != nil {
				logx.Warnf("could not open a browser: %v", err)
			}
		}
		return "", false
	}
	other := inst.App
	if other == "" {
		other = "another program"
	}
	return fmt.Sprintf("%s is already serving %s; free the port or set server.bind_address to another one", other, local), false
}

// -no-browser is added: the page that asked for the restart reloads itself.
func relaunch() {
	exe := desktop.Program()
	if exe == "" {
		logx.Errorf("restart: locating this executable")
		return
	}
	args := os.Args[1:]
	if !slices.Contains(args, "-no-browser") {
		args = append(slices.Clone(args), "-no-browser")
	}
	cmd := exec.Command(exe, args...)
	if err := cmd.Start(); err != nil {
		logx.Errorf("restart: %v", err)
		return
	}
	_ = cmd.Process.Release()
}

// A shortcut launch on Windows has no console, so a startup failure must
// reach a message box.
var dialogOnFatal bool

func reportFatal(msg string) {
	logx.Errorf("FATAL %s", msg)
	if dialogOnFatal {
		desktop.ShowError(appName, msg)
	}
}

// Only for failures early enough that no defer is owed anything.
func fatalf(format string, a ...any) {
	reportFatal(fmt.Sprintf(format, a...))
	os.Exit(1)
}

func explicitFlag(name, value string) string {
	passed := ""
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			passed = value
		}
	})
	return passed
}

func versionLine() string {
	line := appName + " " + internalweb.Version
	if label := internalweb.BuildLabel(); label != "" {
		line += " (" + label + ")"
	}
	return line
}

func openWhenServing(addr string) {
	local := desktop.LoopbackAddr(addr)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if inst, found := desktop.Probe(local, probeTimeout); found && inst.App == appName {
			if err := desktop.OpenBrowser("http://" + local); err != nil {
				logx.Warnf("could not open a browser: %v", err)
			}
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	logx.Warnf("the server did not answer in time; open http://%s yourself", local)
}

func runTray(ctx context.Context, srv *internalweb.Server, bindAddr string) {
	local := desktop.LoopbackAddr(bindAddr)
	hook := srv.DesktopHook()
	err := desktop.RunTray(ctx, desktop.TrayMenu{
		Title:    hook.Name,
		IconPath: trayIconPath(),
		Open: func() {
			if err := desktop.OpenBrowser("http://" + local); err != nil {
				logx.Warnf("could not open a browser: %v", err)
			}
		},
		Quit:      srv.RequestQuit,
		Autostart: hook,
	})
	if err != nil {
		logx.Infof("tray: %v", err)
	}
}

// Windows only: elsewhere the tray names its icon from the desktop theme.
func trayIconPath() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	dir := fsx.ExeDir()
	if dir == "" {
		return ""
	}
	p := filepath.Join(dir, appName+".ico")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}
