package web

import (
	"mime"

	"github.com/monbooru/monbooru/internal/models"
)

// On Windows the registry's MIME types override Go's built-ins.
// AddExtensionType loads the OS table first, so these win.
func init() {
	for _, ft := range models.FileTypes {
		if ft.Kind == "archive" {
			continue
		}
		for _, ext := range ft.Exts {
			_ = mime.AddExtensionType(ext, ft.MIME)
		}
	}
}
