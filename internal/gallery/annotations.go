package gallery

import (
	"database/sql"
	"errors"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

func AnnotationsForImage(database *db.DB, imageID int64) ([]models.Annotation, error) {
	return db.QueryAll(database.Read, func(rows *sql.Rows) (models.Annotation, error) {
		var a models.Annotation
		var manual int
		err := rows.Scan(&a.ID, &a.Site, &a.PostID, &a.X, &a.Y, &a.W, &a.H, &a.Body, &manual)
		a.Manual = manual == 1
		return a, err
	}, `SELECT id, site, post_id, x, y, w, h, body, manual FROM image_annotations WHERE image_id = ? ORDER BY id`, imageID)
}

func ReplaceSourceAnnotations(database *db.DB, imageID int64, site, postID string, boxes []models.Annotation) error {
	return db.InWriteTx(database.Write, func(tx *sql.Tx) error {
		if err := deletePulledAnnotationsTx(tx, imageID, site, postID); err != nil {
			return err
		}
		for _, b := range boxes {
			if _, err := tx.Exec(
				`INSERT INTO image_annotations (image_id, site, post_id, x, y, w, h, body) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				imageID, site, postID, b.X, b.Y, b.W, b.H, b.Body); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddManualAnnotation takes coordinates in original-image pixels,
// validated by the caller.
func AddManualAnnotation(database *db.DB, imageID int64, x, y, w, h int, body string) error {
	_, err := database.Write.Exec(
		`INSERT INTO image_annotations (image_id, site, post_id, x, y, w, h, body, manual) VALUES (?, '', '', ?, ?, ?, ?, ?, 1)`,
		imageID, x, y, w, h, body)
	return err
}

// UpdateAnnotation keeps the manual flag, so a re-pull overwrites an
// edited source box.
func UpdateAnnotation(database *db.DB, imageID, id int64, x, y, w, h int, body string) error {
	res, err := database.Write.Exec(
		`UPDATE image_annotations SET x = ?, y = ?, w = ?, h = ?, body = ? WHERE id = ? AND image_id = ?`,
		x, y, w, h, body, id, imageID)
	return requireAffected(res, err, ErrAnnotationNotFound)
}

func DeleteAnnotation(database *db.DB, imageID, id int64) error {
	res, err := database.Write.Exec(`DELETE FROM image_annotations WHERE id = ? AND image_id = ?`, id, imageID)
	return requireAffected(res, err, ErrAnnotationNotFound)
}

func deletePulledAnnotationsTx(tx *sql.Tx, imageID int64, site, postID string) error {
	_, err := tx.Exec(
		`DELETE FROM image_annotations WHERE image_id = ? AND site = ? AND post_id = ? AND manual = 0`,
		imageID, site, postID)
	return err
}

var ErrAnnotationNotFound = errors.New("annotation not found")

func requireAffected(res sql.Result, err, notFound error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound
	}
	return nil
}
