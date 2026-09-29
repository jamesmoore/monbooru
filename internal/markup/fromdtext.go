package markup

import "strings"

// FromDText converts DText, the markup Danbooru and e621 commentary is
// written in.
func FromDText(src string) string {
	d := &dtext{}
	d.run(src)
	return strings.TrimSpace(d.out.String())
}

type dtext struct {
	out strings.Builder
	// Parse refuses a link inside a link, so a nested one keeps only its label.
	inLink bool
	depth  int
}

var dtextWrappers = map[string]bool{
	"quote": true, "expand": true, "section": true, "spoiler": true,
	"color": true, "nodtext": true,
	"table": true, "thead": true, "tbody": true, "tr": true, "th": true, "td": true,
}

func (d *dtext) run(src string) {
	for i := 0; i < len(src); {
		if d.atLineStart() {
			if w := headerWidth(src[i:]); w > 0 {
				i += w
				continue
			}
		}
		if w := d.construct(src[i:]); w > 0 {
			i += w
			continue
		}
		if src[i] == '\r' {
			d.out.WriteByte('\n')
			if i+1 < len(src) && src[i+1] == '\n' {
				i++
			}
			i++
			continue
		}
		d.out.WriteByte(src[i])
		i++
	}
}

func (d *dtext) construct(s string) int {
	switch {
	case strings.HasPrefix(s, "[["):
		return d.wikiLink(s)
	case s[0] == '[':
		return d.bracketTag(s)
	case strings.HasPrefix(s, "{{"):
		return d.tagSearch(s)
	case s[0] == '"':
		return d.quotedLink(s)
	case s[0] == '<':
		return d.angleURL(s)
	}
	return d.bareURL(s)
}

func (d *dtext) bracketTag(s string) int {
	end := strings.IndexByte(s, ']')
	if end < 0 {
		return 0
	}
	inner := s[1:end]
	closing := strings.HasPrefix(inner, "/")
	name := strings.TrimPrefix(inner, "/")
	// [expand=title] and [section,expanded=true] name the construct first.
	if cut := strings.IndexAny(name, "=,"); cut >= 0 {
		name = name[:cut]
	}
	name = strings.ToLower(strings.TrimSpace(name))
	switch {
	case marks.has(name) && (inner == name || inner == "/"+name):
		d.out.WriteString(s[:end+1])
	case name == "br" && !closing:
		d.out.WriteString("\n")
	case dtextWrappers[name]:
	default:
		return 0
	}
	return end + 1
}

// A wiki page names a tag, with spaces where the tag has underscores.
func (d *dtext) wikiLink(s string) int {
	end := strings.Index(s, "]]")
	if end < 0 {
		return 0
	}
	page, label := splitRef(s[2:end])
	d.tagRef(strings.ReplaceAll(strings.TrimSpace(page), " ", "_"), label)
	return end + 2
}

func (d *dtext) tagSearch(s string) int {
	end := strings.Index(s, "}}")
	if end < 0 {
		return 0
	}
	terms, label := splitRef(s[2:end])
	d.tagRef(strings.TrimSpace(terms), label)
	return end + 2
}

func (d *dtext) quotedLink(s string) int {
	q := strings.IndexByte(s[1:], '"')
	if q < 0 {
		return 0
	}
	label := s[1 : 1+q]
	if strings.ContainsAny(label, "\r\n") {
		return 0
	}
	rest := s[q+2:]
	if !strings.HasPrefix(rest, ":") {
		return 0
	}
	rest = rest[1:]
	var href string
	var w int
	if strings.HasPrefix(rest, "[") {
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			return 0
		}
		href, w = rest[1:end], end+1
	} else if w = urlRunLen(rest); w > 0 {
		href = rest[:w]
	} else {
		return 0
	}
	d.urlRef(href, label)
	return q + 3 + w
}

func (d *dtext) angleURL(s string) int {
	end := strings.IndexByte(s, '>')
	if end < 0 || !validURL(strings.TrimSpace(s[1:end])) {
		return 0
	}
	d.urlRef(s[1:end], s[1:end])
	return end + 1
}

func (d *dtext) bareURL(s string) int {
	if !validURL(s) || !d.atWordStart() {
		return 0
	}
	n := urlRunLen(s)
	if n == 0 {
		return 0
	}
	d.urlRef(s[:n], s[:n])
	return n
}

func (d *dtext) tagRef(name, label string) {
	if d.inLink || !isTagRef(name) {
		d.label(label)
		return
	}
	d.out.WriteString("[tag=" + name + "]")
	d.inLink = true
	d.label(label)
	d.inLink = false
	d.out.WriteString("[/tag]")
}

// A site-relative href has no host to resolve against, so it keeps only
// its label.
func (d *dtext) urlRef(href, label string) {
	href = strings.TrimSpace(href)
	if d.inLink || !validURL(href) || strings.ContainsAny(href, " \t\r\n[]") {
		d.label(label)
		return
	}
	d.out.WriteString("[url=" + href + "]")
	d.inLink = true
	d.label(label)
	d.inLink = false
	d.out.WriteString("[/url]")
}

func (d *dtext) label(s string) {
	if d.depth >= maxDepth {
		d.out.WriteString(s)
		return
	}
	d.depth++
	d.run(s)
	d.depth--
}

func (d *dtext) atLineStart() bool {
	s := d.out.String()
	return s == "" || strings.HasSuffix(s, "\n")
}

func (d *dtext) atWordStart() bool {
	s := d.out.String()
	if s == "" {
		return true
	}
	b := s[len(s)-1]
	return isSpace(b) || strings.IndexByte("([{<\"'", b) >= 0
}

func splitRef(s string) (target, label string) {
	if i := strings.IndexByte(s, '|'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, s
}

func urlRunLen(s string) int {
	i := 0
	for i < len(s) && !isSpace(s[i]) && strings.IndexByte(`<>"[]`, s[i]) < 0 {
		i++
	}
	for i > 0 && strings.IndexByte(".,;:!?", s[i-1]) >= 0 {
		i--
	}
	return i
}

func headerWidth(s string) int {
	if len(s) < 3 || s[0] != 'h' || s[1] < '1' || s[1] > '6' {
		return 0
	}
	i := 2
	if s[i] == '#' {
		for i < len(s) && s[i] != '.' && !isSpace(s[i]) {
			i++
		}
	}
	if i >= len(s) || s[i] != '.' {
		return 0
	}
	for i++; i < len(s) && s[i] == ' '; i++ {
	}
	return i
}
