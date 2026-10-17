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
	return backContextFrom(r.URL.Query())
}

func backContextFrom(q url.Values) backContext {
	return backContext{
		Q:     q.Get("back_q"),
		Sort:  q.Get("back_sort"),
		Order: q.Get("back_order"),
		Page:  q.Get("back_page"),
		Seed:  q.Get("back_seed"),
	}
}

func (b backContext) values(prefix string) url.Values {
	return setNonEmpty(url.Values{}, prefix+"q", b.Q, prefix+"sort", b.Sort, prefix+"order", b.Order,
		prefix+"page", b.Page, prefix+"seed", b.Seed)
}

// pairs alternate key and value; a pair with an empty value is skipped.
func setNonEmpty(v url.Values, pairs ...string) url.Values {
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			v.Set(pairs[i], pairs[i+1])
		}
	}
	return v
}

// setPrefCookie keeps a preference for a year; an empty value deletes it.
func setPrefCookie(w http.ResponseWriter, r *http.Request, name, value string) {
	age := 31_536_000
	if value == "" {
		age = -1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		MaxAge:   age,
		SameSite: http.SameSiteLaxMode,
	})
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
	return fmt.Sprintf("/images/%d", id) + string(b.QueryString("?"))
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
