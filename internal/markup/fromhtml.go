package markup

import (
	"html"
	"net/url"
	"strings"
)

// The tag-name cap: a longer "tags" query is a search and stays a link.
const maxTagRefLen = 200

// FromHTML turns tag-search links on base's host into tag references;
// with a nil base, site-relative links keep only their text.
func FromHTML(src string, base *url.URL) string {
	c := converter{base: base, marks: map[string]int{}}
	c.run(src)
	return strings.TrimSpace(c.out.String())
}

// A dropped link still pushes a frame, with no closing text, so its </a>
// pops it.
type frame struct {
	closing string
	link    bool
}

// links and marks count the stack's frames, so an unmatched close skips
// the walk.
type converter struct {
	base  *url.URL
	out   strings.Builder
	stack []frame
	links int
	marks map[string]int
}

func (c *converter) run(src string) {
	for i := 0; i < len(src); {
		lt := strings.IndexByte(src[i:], '<')
		if lt < 0 {
			c.text(src[i:])
			break
		}
		c.text(src[i : i+lt])
		i += lt
		if w := skipDecl(src[i:]); w > 0 {
			i += w
			continue
		}
		name, closing, attrs, w := scanTag(src[i:])
		if w == 0 {
			c.text("<")
			i++
			continue
		}
		i += w
		if !closing && (name == "script" || name == "style") {
			i += skipElement(src[i:], name)
			continue
		}
		c.tag(name, closing, attrs)
	}
	for len(c.stack) > 0 {
		c.pop()
	}
}

var htmlMarks = map[string]string{
	"b": "b", "strong": "b",
	"i": "i", "em": "i",
	"u": "u", "ins": "u",
	"s": "s", "strike": "s", "del": "s",
	"code": "code", "tt": "code", "pre": "code",
	"tn": "tn",
}

func (c *converter) tag(name string, closing bool, attrs string) {
	if mark, ok := htmlMarks[name]; ok {
		c.mark(mark, closing)
		return
	}
	switch name {
	case "br":
		if !closing {
			c.out.WriteString("\n")
		}
	case "p", "div", "blockquote", "ul", "ol", "table", "tr", "h1", "h2", "h3", "h4", "h5", "h6":
		c.newline()
	case "li":
		c.newline()
		if !closing {
			c.out.WriteString("- ")
		}
	case "a":
		if closing {
			c.closeLink()
		} else {
			c.openLink(attrs)
		}
	}
}

func (c *converter) mark(name string, closing bool) {
	if !closing {
		c.out.WriteString("[" + name + "]")
		c.push(frame{closing: "[/" + name + "]"})
		return
	}
	want := "[/" + name + "]"
	if c.marks[want] == 0 {
		return
	}
	for i := len(c.stack) - 1; i >= 0; i-- {
		if c.stack[i].closing == want && !c.stack[i].link {
			for len(c.stack) > i {
				c.pop()
			}
			return
		}
	}
}

func (c *converter) openLink(attrs string) {
	kind, value := "", ""
	if !c.inLink() {
		kind, value = c.linkTarget(html.UnescapeString(attrValue(attrs, "href")))
	}
	if kind == "" {
		c.push(frame{link: true})
		return
	}
	c.out.WriteString("[" + kind + "=" + value + "]")
	c.push(frame{closing: "[/" + kind + "]", link: true})
}

func (c *converter) closeLink() {
	if c.links == 0 {
		return
	}
	for i := len(c.stack) - 1; i >= 0; i-- {
		if c.stack[i].link {
			for len(c.stack) > i {
				c.pop()
			}
			return
		}
	}
}

func (c *converter) linkTarget(href string) (kind, value string) {
	href = strings.TrimSpace(href)
	if href == "" {
		return "", ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return "", ""
	}
	if c.base != nil {
		u = c.base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", ""
	}
	if c.base != nil && u.Host == c.base.Host {
		if t := u.Query().Get("tags"); isTagRef(t) {
			return "tag", t
		}
	}
	abs := u.String()
	if strings.ContainsAny(abs, "[]") {
		return "", ""
	}
	return "url", abs
}

func isTagRef(s string) bool {
	return s != "" && len([]rune(s)) <= maxTagRefLen &&
		!strings.ContainsAny(s, " \t\r\n[]")
}

func (c *converter) inLink() bool { return c.links > 0 }

func (c *converter) push(f frame) {
	c.stack = append(c.stack, f)
	if f.link {
		c.links++
	} else {
		c.marks[f.closing]++
	}
}

func (c *converter) pop() {
	last := len(c.stack) - 1
	f := c.stack[last]
	c.out.WriteString(f.closing)
	c.stack = c.stack[:last]
	if f.link {
		c.links--
	} else {
		c.marks[f.closing]--
	}
}

func (c *converter) text(s string) {
	if s == "" {
		return
	}
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	c.out.WriteString(strings.ReplaceAll(s, "\r", "\n"))
}

func (c *converter) newline() {
	if s := c.out.String(); s != "" && !strings.HasSuffix(s, "\n") {
		c.out.WriteString("\n")
	}
}

func scanTag(s string) (name string, closing bool, attrs string, width int) {
	i := 1
	if i < len(s) && s[i] == '/' {
		closing = true
		i++
	}
	start := i
	for i < len(s) && isNameByte(s[i]) {
		i++
	}
	if i == start {
		return "", false, "", 0
	}
	name = strings.ToLower(s[start:i])
	attrStart := i
	quote := byte(0)
	for ; i < len(s); i++ {
		switch {
		case quote != 0:
			if s[i] == quote {
				quote = 0
			}
		case s[i] == '"' || s[i] == '\'':
			quote = s[i]
		case s[i] == '>':
			return name, closing, s[attrStart:i], i + 1
		}
	}
	// Unterminated: dropped as a browser does, not rescanned from each '<'.
	return "", false, "", len(s)
}

func isNameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func attrValue(attrs, name string) string {
	for i := 0; i < len(attrs); {
		for i < len(attrs) && isSpace(attrs[i]) {
			i++
		}
		start := i
		for i < len(attrs) && !isSpace(attrs[i]) && attrs[i] != '=' {
			i++
		}
		key := strings.ToLower(attrs[start:i])
		for i < len(attrs) && isSpace(attrs[i]) {
			i++
		}
		if i >= len(attrs) || attrs[i] != '=' {
			continue
		}
		i++
		for i < len(attrs) && isSpace(attrs[i]) {
			i++
		}
		var val string
		if i < len(attrs) && (attrs[i] == '"' || attrs[i] == '\'') {
			quote := attrs[i]
			i++
			start = i
			for i < len(attrs) && attrs[i] != quote {
				i++
			}
			val = attrs[start:i]
			if i < len(attrs) {
				i++
			}
		} else {
			start = i
			for i < len(attrs) && !isSpace(attrs[i]) {
				i++
			}
			val = attrs[start:i]
		}
		if key == name {
			return val
		}
	}
	return ""
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\r' || b == '\n' }

func skipDecl(s string) int {
	if strings.HasPrefix(s, "<!--") {
		if end := strings.Index(s[4:], "-->"); end >= 0 {
			return 4 + end + 3
		}
		return len(s)
	}
	if strings.HasPrefix(s, "<!") || strings.HasPrefix(s, "<?") {
		if end := strings.IndexByte(s, '>'); end >= 0 {
			return end + 1
		}
		return len(s)
	}
	return 0
}

func skipElement(s, name string) int {
	for i := 0; ; {
		j := strings.Index(s[i:], "</")
		if j < 0 {
			return len(s)
		}
		i += j + 2
		if len(s)-i >= len(name) && strings.EqualFold(s[i:i+len(name)], name) {
			if end := strings.IndexByte(s[i:], '>'); end >= 0 {
				return i + end + 1
			}
			return len(s)
		}
	}
}
