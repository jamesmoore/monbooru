package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/shared/desktop"
	"github.com/monbooru/monbooru/internal/shared/logx"
	internalweb "github.com/monbooru/monbooru/internal/web"
)

const appName = "monbooru"

// Stamped "true" at link time (-X) by the desktop artifacts.
var defaultDesktop string

const bindWait = 45 * time.Second

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
	base := cmp.Or(desktop.PicturesDir(), l.DataDir)
	dir := filepath.Join(base, appName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		dir = filepath.Join(l.DataDir, "gallery")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			logx.Warnf("could not create a gallery folder at %s: %v", dir, err)
		}
	}
	return dir
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

func runTray(ctx context.Context, srv *internalweb.Server, bindAddr string) {
	local := desktop.LoopbackAddr(bindAddr)
	hook := srv.DesktopHook()
	err := desktop.RunTray(ctx, desktop.TrayMenu{
		Title:    hook.Name,
		IconPath: desktop.TrayIcon(appName),
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
