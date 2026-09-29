// Package compatibility translates foreign gallery exports into the
// light-import manifest. Each application is one file whose init()
// registers a Provider.
package compatibility

import (
	"archive/zip"
	"fmt"
	"path"
	"strings"
)

// Providers are tried in registration order and the first Detect to match wins.
type Provider struct {
	Name      string
	Detect    func(entries []NormalizedEntry) bool
	Translate func(entries []NormalizedEntry) (Result, error)
}

type Result struct {
	Manifest Manifest
	Files    map[string]*zip.File // key: path under the gallery root
}

// Tags are "name" or "category:name"; the bare form, and an unknown
// category's subtag, land in general.
type Manifest struct {
	Images []ManifestImage
}

// SHA256 may be empty; set it only when it is surely the file's sha256,
// as a merge tags whatever image already has that sha.
type ManifestImage struct {
	SHA256 string
	Path   string
	Tags   []string
}

type NormalizedEntry struct {
	Rel  string
	File *zip.File
}

var providers []Provider

// Register must run from init(): the table has no lock.
func Register(p Provider) { providers = append(providers, p) }

func Detect(files []*zip.File) string {
	entries := NormalizeEntries(files)
	for _, p := range providers {
		if p.Detect(entries) {
			return p.Name
		}
	}
	return ""
}

func Translate(files []*zip.File, format string) (Result, error) {
	entries := NormalizeEntries(files)
	for _, p := range providers {
		if p.Name == format {
			return p.Translate(entries)
		}
	}
	return Result{}, fmt.Errorf("unknown compat format %q", format)
}

// NormalizeEntries strips a top-level directory every entry shares and
// drops directory entries.
func NormalizeEntries(files []*zip.File) []NormalizedEntry {
	var prefix string
	havePrefix := false
	for _, f := range files {
		name := f.Name
		if name == "" {
			continue
		}
		idx := strings.Index(name, "/")
		if idx < 0 {
			havePrefix = false
			break
		}
		head := name[:idx+1]
		if !havePrefix {
			prefix = head
			havePrefix = true
			continue
		}
		if head != prefix {
			havePrefix = false
			break
		}
	}
	out := make([]NormalizedEntry, 0, len(files))
	for _, f := range files {
		rel := f.Name
		if havePrefix {
			rel = strings.TrimPrefix(rel, prefix)
		}
		if rel == "" || strings.HasSuffix(rel, "/") {
			continue
		}
		out = append(out, NormalizedEntry{Rel: rel, File: f})
	}
	return out
}

func HasMediaExt(name string) bool {
	if strings.HasSuffix(name, "/") {
		return false
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".avif", ".jxl", ".gif", ".mp4", ".webm":
		return true
	}
	return false
}

func PickValidSHA256(s string) string {
	if len(s) != 64 {
		return ""
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return s
}
