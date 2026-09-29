package relations

import (
	"database/sql"
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
	if r == nil {
		return false
	}
	if r.DupGroup != nil {
		return true
	}
	if len(r.AltGroupMembers) > 0 {
		return true
	}
	if r.VersionParent != nil || r.VersionChild != nil {
		return true
	}
	if len(r.DerivativeSources) > 0 || len(r.Derivatives) > 0 {
		return true
	}
	return false
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

	var dupGroupID sql.NullInt64
	if err := database.Read.QueryRow(
		`SELECT group_id FROM dup_group_members WHERE image_id = ?`, imageID,
	).Scan(&dupGroupID); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if dupGroupID.Valid {
		var dg DupGroupSummary
		dg.ID = dupGroupID.Int64
		if err := database.Read.QueryRow(
			`SELECT original_image_id FROM dup_groups WHERE id = ?`, dg.ID,
		).Scan(&dg.Original); err != nil {
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

	var altGroupID sql.NullInt64
	if err := database.Read.QueryRow(
		`SELECT group_id FROM alt_group_members WHERE image_id = ?`, imageID,
	).Scan(&altGroupID); err != nil && err != sql.ErrNoRows {
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

	var parentID sql.NullInt64
	if err := database.Read.QueryRow(
		`SELECT parent_image_id FROM version_edges WHERE child_image_id = ?`, imageID,
	).Scan(&parentID); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if parentID.Valid {
		out.VersionParent = &parentID.Int64
	}
	var childID sql.NullInt64
	if err := database.Read.QueryRow(
		`SELECT child_image_id FROM version_edges WHERE parent_image_id = ?`, imageID,
	).Scan(&childID); err != nil && err != sql.ErrNoRows {
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
