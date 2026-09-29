package search

import (
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/relations"
)

type relationPresence struct {
	dup        bool
	alt        bool
	version    bool
	derivative bool
	series     bool
}

func (b *whereBuilder) buildPhashFilter(e FilterExpr) string {
	val := strings.TrimSpace(e.Val)
	if val == "" {
		return "i.phash IS NOT NULL"
	}
	hexPart := val
	distance := -1
	if idx := strings.IndexByte(val, '~'); idx >= 0 {
		hexPart = val[:idx]
		d, err := strconv.Atoi(strings.TrimSpace(val[idx+1:]))
		if err != nil || d < 0 || d > 64 {
			return "1=0"
		}
		distance = d
	}
	hexPart = strings.TrimSpace(hexPart)
	if len(hexPart) != 16 {
		return "1=0"
	}
	u, err := strconv.ParseUint(hexPart, 16, 64)
	if err != nil {
		return "1=0"
	}
	phash := int64(u)
	if distance < 0 {
		b.args = append(b.args, phash)
		return "i.phash = ?"
	}
	if b.db != nil {
		if tree := relations.DefaultRegistry.Lookup(b.db); tree != nil {
			if err := tree.EnsureBuilt(b.db); err == nil {
				// A tree dropped since EnsureBuilt answers built=false;
				// reading its empty result as no matches would be wrong.
				if ids, built := tree.SearchWithinDistance(phash, distance); built {
					if len(ids) == 0 {
						return "1=0"
					}
					return "i.id IN (" + inlineIDs(ids) + ")"
				}
			}
		}
	}
	b.args = append(b.args, phash, distance)
	return "(i.phash IS NOT NULL AND hammingdist(i.phash, ?) <= ?)"
}

const (
	relDupMemberExists = "EXISTS (SELECT 1 FROM dup_group_members m WHERE m.image_id = i.id)"
	relAltMemberExists = "EXISTS (SELECT 1 FROM alt_group_members m WHERE m.image_id = i.id)"
	relVersionExists   = "EXISTS (SELECT 1 FROM version_edges v WHERE v.child_image_id = i.id OR v.parent_image_id = i.id)"
	relSeriesCarrier   = "i.series IS NOT NULL AND i.series != ''"
)

func (b *whereBuilder) buildRelationFilter(e FilterExpr) string {
	val := strings.ToLower(strings.TrimSpace(e.Val))
	b.resolveRelationPresence()
	switch val {
	case "duplicate":
		if !b.relPresence.dup {
			return "1=0"
		}
		return relDupMemberExists
	case "original":
		if !b.relPresence.dup {
			return "1=0"
		}
		return "EXISTS (SELECT 1 FROM dup_groups g WHERE g.original_image_id = i.id)"
	case "alternate":
		if !b.relPresence.alt {
			return "1=0"
		}
		return relAltMemberExists
	case "version":
		if !b.relPresence.version {
			return "1=0"
		}
		return relVersionExists
	case "derivative":
		if !b.relPresence.derivative {
			return "1=0"
		}
		return "EXISTS (SELECT 1 FROM derivative_edges d WHERE d.derivative_image_id = i.id)"
	case "source":
		if !b.relPresence.derivative {
			return "1=0"
		}
		return "EXISTS (SELECT 1 FROM derivative_edges d WHERE d.source_image_id = i.id)"
	case "collection":
		if !b.relPresence.series {
			return "1=0"
		}
		return relSeriesCarrier
	case "any":
		if !b.anyRelationPresent() {
			return "1=0"
		}
		return b.relationAnyClauseForPresence()
	case "none":
		if !b.anyRelationPresent() {
			return "1=1"
		}
		return b.relationNoneClauseForPresence()
	}
	return "1=0"
}

// An empty relation source makes its predicate constant, so it is emitted
// as 1=0 or 1=1 instead of a per-row test.
func (b *whereBuilder) resolveRelationPresence() {
	if b.relPresenceResolved {
		return
	}
	b.relPresenceResolved = true
	if b.db == nil {
		return
	}
	probe := func(query string) bool {
		var has int
		if err := b.db.Read.QueryRow(query).Scan(&has); err != nil {
			// On error assume rows: the full predicate is slow but correct.
			return true
		}
		return has > 0
	}
	b.relPresence = relationPresence{
		dup:        probe(`SELECT EXISTS (SELECT 1 FROM dup_group_members)`),
		alt:        probe(`SELECT EXISTS (SELECT 1 FROM alt_group_members)`),
		version:    probe(`SELECT EXISTS (SELECT 1 FROM version_edges)`),
		derivative: probe(`SELECT EXISTS (SELECT 1 FROM derivative_edges)`),
		series:     probe(`SELECT EXISTS (SELECT 1 FROM images WHERE series IS NOT NULL AND series != '' LIMIT 1)`),
	}
}

func (b *whereBuilder) anyRelationPresent() bool {
	p := &b.relPresence
	return p.dup || p.alt || p.version || p.derivative || p.series
}

func (b *whereBuilder) relationAnyClauseForPresence() string {
	p := &b.relPresence
	parts := make([]string, 0, 5)
	if p.dup {
		parts = append(parts,
			relDupMemberExists,
		)
	}
	if p.alt {
		parts = append(parts,
			relAltMemberExists,
		)
	}
	if p.version {
		parts = append(parts,
			relVersionExists,
		)
	}
	if p.derivative {
		parts = append(parts,
			"EXISTS (SELECT 1 FROM derivative_edges d WHERE d.derivative_image_id = i.id OR d.source_image_id = i.id)",
		)
	}
	if p.series {
		parts = append(parts, "("+relSeriesCarrier+")")
	}
	if len(parts) == 0 {
		return "1=0"
	}
	if len(parts) == 1 {
		return parts[0]
	}
	// Parenthesised: the caller ANDs the visible guard on, and AND binds
	// tighter than OR.
	return "(" + strings.Join(parts, " OR ") + ")"
}

func (b *whereBuilder) relationNoneClauseForPresence() string {
	p := &b.relPresence
	var unions []string
	if p.dup {
		unions = append(unions, "SELECT image_id FROM dup_group_members")
	}
	if p.alt {
		unions = append(unions, "SELECT image_id FROM alt_group_members")
	}
	if p.version {
		unions = append(unions,
			"SELECT child_image_id FROM version_edges",
			"SELECT parent_image_id FROM version_edges",
		)
	}
	if p.derivative {
		unions = append(unions,
			"SELECT derivative_image_id FROM derivative_edges",
			"SELECT source_image_id FROM derivative_edges",
		)
	}
	if !p.series && len(unions) == 0 {
		return "1=1"
	}
	// Inside the NOT IN the carriers come off the partial idx_images_series;
	// tested per row, every visible image's series would be read.
	if p.series {
		unions = append(unions, "SELECT id FROM images WHERE series IS NOT NULL AND series != ''")
	}
	return "i.id NOT IN (\n\t\t" + strings.Join(unions, "\n\t\tUNION ") + "\n\t)"
}
