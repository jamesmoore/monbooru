package gallery

import (
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/tags"
)

type TagRefTarget struct {
	Color string
	Found bool
}

type tagRefRow struct {
	name     string
	category string
	color    string
	usage    int
}

// ResolveTagRefs gives an alias its canonical row's colour, so the swatch
// matches where the link lands. A category prefix is read first, then the
// whole ref as a name in any category.
func ResolveTagRefs(database *db.DB, refs []string) map[string]TagRefTarget {
	out := make(map[string]TagRefTarget, len(refs))
	forms := make([]tags.Form, len(refs))
	var names []string
	for i, ref := range refs {
		out[ref] = TagRefTarget{}
		names = append(names, tags.NormalizeTagName(ref))
		if f, err := tags.SplitForm(database, ref); err == nil && f.CategoryID != 0 && f.Name != "" {
			forms[i] = f
			names = append(names, tags.NormalizeTagName(f.Name))
		}
	}
	placeholders, args := db.InPlaceholders(names)
	if placeholders == "" {
		return out
	}
	rows, err := db.QueryAll(database.Read, db.ScanRow(func(r *tagRefRow) []any {
		return []any{&r.name, &r.category, &r.color, &r.usage}
	}), `SELECT t.name, tc.name, COALESCE(cc.color, tc.color), COALESCE(ct.usage_count, t.usage_count)
	    FROM tags t
	    JOIN tag_categories tc ON tc.id = t.category_id
	    LEFT JOIN tags ct ON ct.id = t.canonical_tag_id
	    LEFT JOIN tag_categories cc ON cc.id = ct.category_id
	    WHERE t.name IN (`+placeholders+`)`, args...)
	if err != nil {
		return out
	}
	for i, ref := range refs {
		if f := forms[i]; f.CategoryID != 0 {
			if best, ok := pickTagRef(rows, tags.NormalizeTagName(f.Name), f.Category); ok {
				out[ref] = best
				continue
			}
		}
		if best, ok := pickTagRef(rows, tags.NormalizeTagName(ref), ""); ok {
			out[ref] = best
		}
	}
	return out
}

func pickTagRef(rows []tagRefRow, name, category string) (TagRefTarget, bool) {
	best, found := tagRefRow{usage: -1}, false
	for _, r := range rows {
		if r.name != name || (category != "" && r.category != category) {
			continue
		}
		if r.usage > best.usage {
			best, found = r, true
		}
	}
	return TagRefTarget{Color: best.color, Found: found}, found
}

func ExistingImageIDs(database *db.DB, ids []int64) map[int64]bool {
	out := map[int64]bool{}
	placeholders, args := db.InPlaceholders(ids)
	if placeholders == "" {
		return out
	}
	found, err := db.QueryIDs(database.Read, `SELECT id FROM images WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return out
	}
	for _, id := range found {
		out[id] = true
	}
	return out
}

func ImageIDsBySourceURL(database *db.DB, urls []string) map[string]int64 {
	out := map[string]int64{}
	placeholders, args := db.InPlaceholders(urls)
	if placeholders == "" {
		return out
	}
	type hit struct {
		url string
		id  int64
	}
	hits, err := db.QueryAll(database.Read, db.ScanRow(func(h *hit) []any {
		return []any{&h.url, &h.id}
	}), `SELECT url, MIN(image_id) FROM image_sources WHERE url IN (`+placeholders+`) GROUP BY url`, args...)
	if err != nil {
		return out
	}
	for _, h := range hits {
		out[h.url] = h.id
	}
	return out
}
