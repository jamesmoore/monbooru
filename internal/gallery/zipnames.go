package gallery

import (
	"path/filepath"
	"strings"
)

// ZipNamer compares names ignoring case: the zip is usually unpacked on a
// filesystem that folds it.
type ZipNamer struct {
	taken map[string]bool
}

func NewZipNamer(reserved ...string) *ZipNamer {
	z := &ZipNamer{taken: make(map[string]bool, len(reserved))}
	for _, name := range reserved {
		z.taken[strings.ToLower(name)] = true
	}
	return z
}

func (z *ZipNamer) Name(path string) string {
	name := SanitizeFilename(filepath.Base(path))
	ext := filepath.Ext(name)
	stem := TruncateFilename(strings.TrimSuffix(name, ext), maxNameBytes)
	entry := stem + ext
	for i := 1; z.taken[strings.ToLower(entry)]; i++ {
		entry = uploadSuffix(stem, ext, i)
	}
	z.taken[strings.ToLower(entry)] = true
	return entry
}
