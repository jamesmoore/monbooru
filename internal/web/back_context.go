package web

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
)

type backContext struct {
	Q     string
	Sort  string
	Order string
	Page  string
	Seed  string
}

func parseBackContext(r *http.Request) backContext {
	q := r.URL.Query()
	return backContext{
		Q:     q.Get("back_q"),
		Sort:  q.Get("back_sort"),
		Order: q.Get("back_order"),
		Page:  q.Get("back_page"),
		Seed:  q.Get("back_seed"),
	}
}

func (b backContext) values(prefix string) url.Values {
	v := url.Values{}
	for _, f := range []struct{ key, val string }{
		{"q", b.Q},
		{"sort", b.Sort},
		{"order", b.Order},
		{"page", b.Page},
		{"seed", b.Seed},
	} {
		if f.val != "" {
			v.Set(prefix+f.key, f.val)
		}
	}
	return v
}

func (b backContext) URLValues() url.Values { return b.values("back_") }

// template.URL so html/template does not escape the & separators.
func (b backContext) QueryString(sep string) template.URL {
	v := b.URLValues()
	if len(v) == 0 {
		return ""
	}
	return template.URL(sep + v.Encode())
}

func (b backContext) DetailURL(id int64) string {
	base := fmt.Sprintf("/images/%d", id)
	v := b.URLValues()
	if len(v) == 0 {
		return base
	}
	return base + "?" + v.Encode()
}

func (b backContext) GalleryURL() string {
	if b == (backContext{}) {
		return "/"
	}
	return "/?" + b.values("").Encode()
}

func (b backContext) ReaderQS(fromPages bool) (template.URL, template.URL) {
	v := b.URLValues()
	if fromPages {
		v.Set("from", "pages")
	}
	if len(v) == 0 {
		return "", ""
	}
	enc := v.Encode()
	return template.URL("?" + enc), template.URL("&" + enc)
}
