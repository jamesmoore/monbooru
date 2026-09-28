package gallery

import (
	"database/sql"
	"errors"
	"sort"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

// images.series and series_order mirror one "home" membership so the
// order sort and the adjacency cursor can ride scalar columns:
// series != '' iff the image has a membership, and then names one of them.

func orderValue(order *int) any {
	if order == nil {
		return nil
	}
	return *order
}

func CollectionsForImage(database *db.DB, imageID int64) ([]models.Collection, error) {
	return db.QueryAll(database.Read, func(rows *sql.Rows) (models.Collection, error) {
		var c models.Collection
		var pos sql.NullInt64
		err := rows.Scan(&c.Name, &pos)
		if pos.Valid {
			v := int(pos.Int64)
			c.Order = &v
		}
		return c, err
	}, `SELECT name, position FROM image_collections WHERE image_id = ?
		 ORDER BY position IS NULL, position, name`, imageID)
}

func AddCollectionMembership(database *db.DB, imageID int64, name string, order *int) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("collection name required")
	}
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		return addMembershipTx(tx, imageID, name, order)
	})
}

func addMembershipTx(tx *sql.Tx, imageID int64, name string, order *int) error {
	if _, err := tx.Exec(
		`INSERT INTO image_collections (image_id, name, position) VALUES (?, ?, ?)
		 ON CONFLICT(image_id, name) DO UPDATE SET position = excluded.position`,
		imageID, name, orderValue(order)); err != nil {
		return err
	}
	home, err := homeName(tx, imageID)
	if err != nil {
		return err
	}
	switch {
	case home == "":
		_, err = tx.Exec(`UPDATE images SET series = ?, series_order = ? WHERE id = ?`,
			name, orderValue(order), imageID)
	case strings.EqualFold(home, name):
		_, err = tx.Exec(`UPDATE images SET series_order = ? WHERE id = ?`,
			orderValue(order), imageID)
	}
	return err
}

func RemoveCollectionMembership(database *db.DB, imageID int64, name string) error {
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		return removeMembershipTx(tx, imageID, name)
	})
}

func removeMembershipTx(tx *sql.Tx, imageID int64, name string) error {
	if _, err := tx.Exec(`DELETE FROM image_collections WHERE image_id = ? AND name = ?`,
		imageID, name); err != nil {
		return err
	}
	return rebindHomeTx(tx, imageID, name)
}

func RenameCollectionMembership(database *db.DB, imageID int64, prev, name string, order *int) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("collection name required")
	}
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		if err := removeMembershipTx(tx, imageID, prev); err != nil {
			return err
		}
		return addMembershipTx(tx, imageID, name, order)
	})
}

// SetHomeCollection promotes a name the image already holds and keeps the
// old home as a membership; a new name, or "", drops the old home.
func SetHomeCollection(database *db.DB, imageID int64, name string, order *int) error {
	name = strings.TrimSpace(name)
	tx, err := database.Write.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	oldHome, err := homeName(tx, imageID)
	if err != nil {
		return err
	}
	relabel := oldHome != "" && !strings.EqualFold(oldHome, name)
	if relabel && name != "" {
		var x int
		switch err := tx.QueryRow(
			`SELECT 1 FROM image_collections WHERE image_id = ? AND name = ?`, imageID, name).Scan(&x); {
		case err == nil:
			relabel = false
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
	}
	if relabel {
		if _, err := tx.Exec(`DELETE FROM image_collections WHERE image_id = ? AND name = ?`,
			imageID, oldHome); err != nil {
			return err
		}
	}
	if name == "" {
		if err := rebindHomeTx(tx, imageID, oldHome); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.Exec(
		`INSERT INTO image_collections (image_id, name, position) VALUES (?, ?, ?)
		 ON CONFLICT(image_id, name) DO UPDATE SET position = excluded.position`,
		imageID, name, orderValue(order)); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE images SET series = ?, series_order = ? WHERE id = ?`,
		name, orderValue(order), imageID); err != nil {
		return err
	}
	return tx.Commit()
}

func homeName(tx *sql.Tx, imageID int64) (string, error) {
	var home sql.NullString
	if err := tx.QueryRow(`SELECT series FROM images WHERE id = ?`, imageID).Scan(&home); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if !home.Valid {
		return "", nil
	}
	return home.String, nil
}

// Runs after changedName's row is deleted, or it could promote that row again.
func rebindHomeTx(tx *sql.Tx, imageID int64, changedName string) error {
	home, err := homeName(tx, imageID)
	if err != nil {
		return err
	}
	if home == "" || !strings.EqualFold(home, changedName) {
		return nil
	}
	var name sql.NullString
	var pos sql.NullInt64
	err = tx.QueryRow(
		`SELECT name, position FROM image_collections WHERE image_id = ?
		 ORDER BY position IS NULL, position, name LIMIT 1`, imageID).Scan(&name, &pos)
	if errors.Is(err, sql.ErrNoRows) {
		_, e := tx.Exec(`UPDATE images SET series = '', series_order = NULL WHERE id = ?`, imageID)
		return e
	}
	if err != nil {
		return err
	}
	var ord any
	if pos.Valid {
		ord = pos.Int64
	}
	_, e := tx.Exec(`UPDATE images SET series = ?, series_order = ? WHERE id = ?`,
		name.String, ord, imageID)
	return e
}

type CollectionSummary struct {
	Name          string
	Count         int
	FindRelations bool
	Samples       []CollectionSample
}

type CollectionSample struct {
	ID       int64
	Order    *int
	Filename string
}

func collectionFilterWhere(col, nameFilter string) (string, []any) {
	if nameFilter == "" {
		return "", nil
	}
	return ` AND ` + col + ` LIKE ? ESCAPE '\'`, []any{"%" + db.EscapeLike(nameFilter) + "%"}
}

func ListCollections(database *db.DB, nameFilter, sort string, limit, offset int, excludeIDs []int64) ([]CollectionSummary, error) {
	where, filterArgs := collectionFilterWhere("c.name", nameFilter)
	var query string
	var args []any
	if len(excludeIDs) == 0 {
		// The trigger-maintained counts read one row per label instead of
		// walking every membership.
		orderBy := "c.visible_count DESC, c.name ASC"
		if sort == "name" {
			orderBy = "c.name ASC"
		}
		query = `SELECT c.name, c.visible_count,
		        EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = c.name)
		 FROM collection_counts c
		 WHERE c.visible_count > 0` + where + `
		 ORDER BY ` + orderBy + ` LIMIT ? OFFSET ?`
		args = append(filterArgs, limit, offset)
	} else {
		// The stored counts are ceiling-blind. EXISTS rather than a join
		// lets the GROUP BY stream off idx_image_collections_name instead
		// of temp-sorting every member.
		exclude, excludeArgs := excludeNotExists("c.image_id", excludeIDs)
		orderBy := "cnt DESC, c.name ASC"
		if sort == "name" {
			orderBy = "c.name ASC"
		}
		query = `SELECT c.name, COUNT(*) cnt,
		        EXISTS (SELECT 1 FROM collection_find_relations f WHERE f.name = c.name)
		 FROM image_collections c
		 WHERE EXISTS (SELECT 1 FROM images i WHERE i.id = c.image_id AND i.is_missing = 0)` + exclude + where + `
		 GROUP BY c.name ORDER BY ` + orderBy + ` LIMIT ? OFFSET ?`
		args = append(append(excludeArgs, filterArgs...), limit, offset)
	}
	return db.QueryAll(database.Read, func(rows *sql.Rows) (CollectionSummary, error) {
		var c CollectionSummary
		err := rows.Scan(&c.Name, &c.Count, &c.FindRelations)
		return c, err
	}, query, args...)
}

func SetCollectionFindRelations(database *db.DB, name string, enabled bool) error {
	if enabled {
		_, err := database.Write.Exec(
			`INSERT OR IGNORE INTO collection_find_relations (name) VALUES (?)`, name)
		return err
	}
	_, err := database.Write.Exec(
		`DELETE FROM collection_find_relations WHERE name = ?`, name)
	return err
}

func CountCollections(database *db.DB, nameFilter string, excludeIDs []int64) (int, error) {
	var n int
	if len(excludeIDs) == 0 {
		where, args := collectionFilterWhere("name", nameFilter)
		err := database.Read.QueryRow(
			`SELECT COUNT(*) FROM collection_counts WHERE visible_count > 0`+where, args...).Scan(&n)
		return n, err
	}
	// The per-label EXISTS stops at the first visible member, so the cost
	// tracks labels, not memberships.
	exclude, args := excludeNotExists("c.image_id", excludeIDs)
	where, filterArgs := collectionFilterWhere("d.name", nameFilter)
	args = append(args, filterArgs...)
	err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM (SELECT DISTINCT name FROM image_collections) d
		 WHERE EXISTS (SELECT 1 FROM image_collections c JOIN images i ON i.id = c.image_id
		   WHERE c.name = d.name AND i.is_missing = 0`+exclude+`)`+where, args...).Scan(&n)
	return n, err
}

// CollectionSamples keys by the lower-cased label: NOCASE labels may be
// stored in several cases. A LIMITed query per name stops each index walk
// early, where a ROW_NUMBER window would rank every member.
func CollectionSamples(database *db.DB, names []string, per int, excludeIDs []int64) (map[string][]CollectionSample, error) {
	out := make(map[string][]CollectionSample, len(names))
	if len(names) == 0 || per <= 0 {
		return out, nil
	}
	for _, name := range names {
		samples, err := CollectionMembers(database, name, excludeIDs, per, 0)
		if err != nil {
			return out, err
		}
		key := strings.ToLower(name)
		if len(out[key]) == 0 {
			out[key] = samples
		}
	}
	return out, nil
}

// CollectionMembers' ORDER BY matches idx_image_collections_reading, so
// the LIMIT stops the scan early instead of sorting the whole label.
func CollectionMembers(database *db.DB, name string, excludeIDs []int64, limit, offset int) ([]CollectionSample, error) {
	exclude, args := excludeNotExists("i.id", excludeIDs)
	args = append([]any{name}, args...)
	args = append(args, limit, offset)
	return db.QueryAll(database.Read, func(rows *sql.Rows) (CollectionSample, error) {
		var sample CollectionSample
		var pos sql.NullInt64
		err := rows.Scan(&sample.ID, &pos, &sample.Filename)
		if pos.Valid {
			v := int(pos.Int64)
			sample.Order = &v
		}
		return sample, err
	},
		`SELECT c.image_id, c.position, basename(i.canonical_path)
		 FROM image_collections c JOIN images i ON i.id = c.image_id
		 WHERE c.name = ? AND i.is_missing = 0`+exclude+`
		 ORDER BY c.position IS NULL, c.position, c.image_id LIMIT ? OFFSET ?`, args...)
}

// ReorderCollection leaves the members missing from ids unordered. Past
// one chunk the writes are split, so a large reorder is not atomic.
func ReorderCollection(database *db.DB, name string, ids []int64) error {
	const chunkSize = 500
	const clearAll = `UPDATE image_collections SET position = NULL WHERE name = ?`
	const setPos = `UPDATE image_collections SET position = ? WHERE image_id = ? AND name = ?`
	const resync = `UPDATE images SET series_order =
		   (SELECT position FROM image_collections WHERE image_id = images.id AND name = ?)
		 WHERE series = ? COLLATE NOCASE`

	if len(ids) <= chunkSize {
		return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
			if _, err := tx.Exec(clearAll, name); err != nil {
				return err
			}
			for i, id := range ids {
				if _, err := tx.Exec(setPos, i+1, id, name); err != nil {
					return err
				}
			}
			_, err := tx.Exec(resync, name, name)
			return err
		})
	}

	if err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		_, e := tx.Exec(clearAll, name)
		return e
	}); err != nil {
		return err
	}
	for start := 0; start < len(ids); start += chunkSize {
		lo, hi := start, min(start+chunkSize, len(ids))
		if err := db.InWriteTx(database.Write, func(tx *sql.Tx) error {
			for i := lo; i < hi; i++ {
				if _, err := tx.Exec(setPos, i+1, ids[i], name); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		_, e := tx.Exec(resync, name, name)
		return e
	})
}

func SortCollectionByFilename(database *db.DB, name string) error {
	type member struct {
		id       int64
		filename string
	}
	members, err := db.QueryAll(database.Read, func(rows *sql.Rows) (member, error) {
		var m member
		err := rows.Scan(&m.id, &m.filename)
		return m, err
	}, `SELECT c.image_id, basename(i.canonical_path)
		 FROM image_collections c JOIN images i ON i.id = c.image_id
		 WHERE c.name = ? AND i.is_missing = 0
		 ORDER BY c.image_id`, name)
	if err != nil {
		return err
	}
	sort.SliceStable(members, func(i, j int) bool {
		return NaturalLess(strings.ToLower(members[i].filename), strings.ToLower(members[j].filename))
	})
	ids := make([]int64, len(members))
	for i, m := range members {
		ids[i] = m.id
	}
	return ReorderCollection(database, name, ids)
}

func CollectionCeilingHidden(database *db.DB, name string, excludeIDs []int64) (int, error) {
	if len(excludeIDs) == 0 {
		return 0, nil
	}
	var blind int
	err := database.Read.QueryRow(
		`SELECT COALESCE(visible_count, 0) FROM collection_counts WHERE name = ?`, name).Scan(&blind)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	exclude, exArgs := excludeNotExists("c.image_id", excludeIDs)
	var filtered int
	if err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM image_collections c
		 WHERE c.name = ? AND EXISTS (SELECT 1 FROM images i WHERE i.id = c.image_id AND i.is_missing = 0)`+exclude,
		append([]any{name}, exArgs...)...).Scan(&filtered); err != nil {
		return 0, err
	}
	if blind < filtered {
		return 0, nil
	}
	return blind - filtered, nil
}

// CollectionHiddenOrderedIDs must be appended to a ceiling-filtered
// reorder's ids, or ReorderCollection clears positions the dialog never
// showed.
func CollectionHiddenOrderedIDs(database *db.DB, name string, excludeIDs []int64) ([]int64, error) {
	if len(excludeIDs) == 0 {
		return nil, nil
	}
	placeholders, args := db.InPlaceholders(excludeIDs)
	return db.QueryIDs(database.Read,
		`SELECT c.image_id FROM image_collections c JOIN images i ON i.id = c.image_id
		 WHERE c.name = ? AND c.position IS NOT NULL AND i.is_missing = 0
		   AND EXISTS (SELECT 1 FROM image_tags it WHERE it.image_id = c.image_id AND it.tag_id IN (`+placeholders+`))
		 ORDER BY c.position, c.image_id`,
		append([]any{name}, args...)...)
}

// CollectionMemberIDs includes missing rows, so a rename or dissolve
// reaches the whole collection.
func CollectionMemberIDs(database *db.DB, name string) ([]int64, error) {
	return db.QueryIDs(database.Read,
		`SELECT image_id FROM image_collections WHERE name = ? COLLATE NOCASE`, name)
}

func CollectionCBZMembers(database *db.DB, name string) ([]CBZMember, error) {
	rows, err := database.Read.Query(
		`SELECT i.canonical_path, i.file_type, basename(i.canonical_path), c.position
		 FROM image_collections c JOIN images i ON i.id = c.image_id
		 WHERE c.name = ? AND i.is_missing = 0
		 ORDER BY c.position IS NULL, c.position, c.image_id`, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out, unordered []CBZMember
	for rows.Next() {
		var m CBZMember
		var pos sql.NullInt64
		if err := rows.Scan(&m.Path, &m.FileType, &m.filename, &pos); err != nil {
			return nil, err
		}
		if pos.Valid {
			out = append(out, m)
		} else {
			unordered = append(unordered, m)
		}
	}
	sort.SliceStable(unordered, func(i, j int) bool {
		return NaturalLess(strings.ToLower(unordered[i].filename), strings.ToLower(unordered[j].filename))
	})
	return append(out, unordered...), rows.Err()
}

func AddCollectionToImages(database *db.DB, ids []int64, name string) error {
	placeholders, args := db.InPlaceholders(ids)
	labelArgs := append([]any{name}, args...)
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`INSERT INTO image_collections (image_id, name, position)
			 SELECT id, ?, NULL FROM images WHERE id IN (`+placeholders+`)
			 ON CONFLICT(image_id, name) DO NOTHING`,
			labelArgs...,
		); err != nil {
			return err
		}
		_, err := tx.Exec(
			`UPDATE images SET series = ?, series_order = NULL
			 WHERE series = '' AND id IN (`+placeholders+`)`,
			labelArgs...,
		)
		return err
	})
}

func RemoveCollectionFromImages(database *db.DB, ids []int64, name string) error {
	placeholders, args := db.InPlaceholders(ids)
	labelArgs := append([]any{name}, args...)
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`DELETE FROM image_collections WHERE name = ? AND image_id IN (`+placeholders+`)`,
			labelArgs...,
		); err != nil {
			return err
		}
		_, err := tx.Exec(
			`UPDATE images SET
			   series = COALESCE((SELECT name FROM image_collections c WHERE c.image_id = images.id
			                      ORDER BY c.position IS NULL, c.position, c.name LIMIT 1), ''),
			   series_order = (SELECT position FROM image_collections c WHERE c.image_id = images.id
			                   ORDER BY c.position IS NULL, c.position, c.name LIMIT 1)
			 WHERE series = ? COLLATE NOCASE AND id IN (`+placeholders+`)`,
			labelArgs...,
		)
		return err
	})
}

// RenameCollectionForImages, when merging, first drops oldName where the
// image already holds newName, so the rename cannot collide and the
// existing position wins the home resync.
func RenameCollectionForImages(database *db.DB, ids []int64, oldName, newName string, posOffset int, merging bool) error {
	placeholders, args := db.InPlaceholders(ids)
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		if merging {
			if _, err := tx.Exec(
				`DELETE FROM image_collections
				 WHERE name = ? AND image_id IN (`+placeholders+`)
				   AND image_id IN (SELECT image_id FROM image_collections WHERE name = ?)`,
				append(append([]any{oldName}, args...), newName)...,
			); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(
			`UPDATE image_collections SET name = ?, position = position + ?
			 WHERE name = ? AND image_id IN (`+placeholders+`)`,
			append([]any{newName, posOffset, oldName}, args...)...,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE images SET series = ? WHERE series = ? COLLATE NOCASE AND id IN (`+placeholders+`)`,
			append([]any{newName, oldName}, args...)...,
		); err != nil {
			return err
		}
		_, err := tx.Exec(
			`UPDATE images SET series_order =
			   (SELECT position FROM image_collections WHERE image_id = images.id AND name = ?)
			 WHERE series = ? COLLATE NOCASE AND id IN (`+placeholders+`)`,
			append([]any{newName, newName}, args...)...,
		)
		return err
	})
}
