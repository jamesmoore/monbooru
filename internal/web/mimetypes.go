package web

import (
	"mime"

	"github.com/monbooru/monbooru/internal/models"
)

// On Windows the registry's MIME types override Go's built-ins, and a
// .css registered as text/plain breaks the stylesheet. AddExtensionType
// loads the OS table first, so these win.
func init() {
	_ = mime.AddExtensionType(".css", "text/css; charset=utf-8")
	_ = mime.AddExtensionType(".js", "text/javascript; charset=utf-8")
	for _, ft := range models.FileTypes {
		if ft.Kind == "archive" {
			continue
		}
		for _, ext := range ft.Exts {
			_ = mime.AddExtensionType(ext, ft.MIME)
		}
	}
}
