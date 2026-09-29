package web

import "mime"

// On Windows the registry's MIME types override Go's built-ins, and a
// .css registered as text/plain breaks the stylesheet. AddExtensionType
// loads the OS table first, so these win.
func init() {
	for ext, typ := range map[string]string{
		".css":  "text/css; charset=utf-8",
		".js":   "text/javascript; charset=utf-8",
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".gif":  "image/gif",
		".webp": "image/webp",
		".avif": "image/avif",
		".jxl":  "image/jxl",
		".mp4":  "video/mp4",
		".webm": "video/webm",
	} {
		_ = mime.AddExtensionType(ext, typ)
	}
}
