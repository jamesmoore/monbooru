package tagger

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/monbooru/monbooru/internal/fsx"
)

// sharedLibPath tries the executable's directory first so a bundled
// library wins over the system one.
func sharedLibPath() string {
	if p := os.Getenv("ORT_LIB_PATH"); p != "" {
		return p
	}
	name := "libonnxruntime.so"
	var candidates []string
	if runtime.GOOS == "windows" {
		name = "onnxruntime.dll"
	}
	if dir := fsx.ExeDir(); dir != "" {
		candidates = append(candidates, filepath.Join(dir, name))
	}
	if runtime.GOOS == "windows" {
		// The working directory is a Windows loader convention.
		if wd, err := os.Getwd(); err == nil {
			candidates = append(candidates, filepath.Join(wd, name))
		}
	} else {
		candidates = append(candidates, "/usr/lib/"+name, "/usr/local/lib/"+name)
	}
	candidates = append(candidates, name)
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return name
}

func missingRuntimeLibrary() string {
	path := sharedLibPath()
	if _, err := os.Stat(path); err == nil {
		return ""
	}
	reason := "missing " + filepath.Base(path)
	// A bare name means nothing was found anywhere.
	if path == filepath.Base(path) {
		if dir := fsx.ExeDir(); dir != "" {
			return reason + " (expected in " + dir + ")"
		}
		return reason
	}
	return reason + " at " + path
}
