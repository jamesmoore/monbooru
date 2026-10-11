package relations

import (
	"database/sql"
	"errors"
	"slices"

	"github.com/monbooru/monbooru/internal/db"
)

type ImageRelations struct {
	DupGroup          *DupGroupSummary
	AltGroupID        *int64
	AltGroupMembers   []int64
	VersionParent     *int64
	VersionChild      *int64
	DerivativeSources []int64
	Derivatives       []int64
}

type DupGroupSummary struct {
	ID       int64
	Original int64
	Members  []int64
}

func (r *ImageRelations) HasAny() bool {
	return r != nil && (r.DupGroup != nil || len(r.AltGroupMembers) > 0 ||
		r.VersionParent != nil || r.VersionChild != nil ||
		len(r.DerivativeSources) > 0 || len(r.Derivatives) > 0)
}

// CommonDerivativeAncestor searches the whole ancestry, not just direct
// sources: cousins share tree context too.
func CommonDerivativeAncestor(database *db.DB, a, b int64) (int64, bool, error) {
	up, err := derivativeAncestors(database, a)
	if err != nil || len(up) == 0 {
		return 0, false, err
	}
	other, err := derivativeAncestors(database, b)
	if err != nil {
		return 0, false, err
	}
	for _, id := range up {
		if slices.Contains(other, id) {
			return id, true, nil
		}
	}
	return 0, false, nil
}

// Nearest first: chainSpan walks breadth-first.
func derivativeAncestors(database *db.DB, imageID int64) ([]int64, error) {
	above, _, err := chainSpan(database.Read, "derivative_edges", "source_image_id", "derivative_image_id", imageID)
	if err != nil {
		return nil, err
	}
	return above[1:], nil
}

func HasDerivativeSource(database *db.DB, imageID int64) (bool, error) {
	var has int
	err := database.Read.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM derivative_edges WHERE derivative_image_id = ?)`, imageID,
	).Scan(&has)
	return has != 0, err
}

func LoadImageRelations(database *db.DB, imageID int64) (*ImageRelations, error) {
	out := &ImageRelations{}

	dupGroupID, err := optionalID(database.Read, `SELECT group_id FROM dup_group_members WHERE image_id = ?`, imageID)
	if err != nil {
		return nil, err
	}
	if dupGroupID.Valid {
		dg := DupGroupSummary{ID: dupGroupID.Int64}
		if dg.Original, err = GroupOriginal(database.Read, dg.ID); err != nil {
			return nil, err
		}
		members, err := db.QueryIDs(database.Read,
			`SELECT image_id FROM dup_group_members WHERE group_id = ? ORDER BY image_id`, dg.ID,
		)
		if err != nil {
			return nil, err
		}
		dg.Members = members
		out.DupGroup = &dg
	}

	altGroupID, err := optionalID(database.Read, `SELECT group_id FROM alt_group_members WHERE image_id = ?`, imageID)
	if err != nil {
		return nil, err
	}
	if altGroupID.Valid {
		out.AltGroupID = &altGroupID.Int64
		members, err := db.QueryIDs(database.Read,
			`SELECT image_id FROM alt_group_members WHERE group_id = ? ORDER BY image_id`,
			altGroupID.Int64,
		)
		if err != nil {
			return nil, err
		}
		out.AltGroupMembers = members
	}

	parentID, err := optionalID(database.Read, `SELECT parent_image_id FROM version_edges WHERE child_image_id = ?`, imageID)
	if err != nil {
		return nil, err
	}
	if parentID.Valid {
		out.VersionParent = &parentID.Int64
	}
	childID, err := optionalID(database.Read, `SELECT child_image_id FROM version_edges WHERE parent_image_id = ?`, imageID)
	if err != nil {
		return nil, err
	}
	if childID.Valid {
		out.VersionChild = &childID.Int64
	}

	sources, err := db.QueryIDs(database.Read,
		`SELECT source_image_id FROM derivative_edges WHERE derivative_image_id = ? ORDER BY source_image_id`, imageID,
	)
	if err != nil {
		return nil, err
	}
	out.DerivativeSources = sources
	derivatives, err := db.QueryIDs(database.Read,
		`SELECT derivative_image_id FROM derivative_edges WHERE source_image_id = ? ORDER BY derivative_image_id`, imageID,
	)
	if err != nil {
		return nil, err
	}
	out.Derivatives = derivatives
	return out, nil
}

// No row reads as a NULL id.
func optionalID(q db.RowQuerier, query string, arg int64) (sql.NullInt64, error) {
	var id sql.NullInt64
	err := q.QueryRow(query, arg).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return id, nil
	}
	return id, err
}

func GroupOriginal(q db.RowQuerier, groupID int64) (int64, error) {
	var original int64
	err := q.QueryRow(`SELECT original_image_id FROM dup_groups WHERE id = ?`, groupID).Scan(&original)
	return original, err
}
