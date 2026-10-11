package search

import (
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

// SaveSearch refuses a taken name rather than overwriting that search.
func SaveSearch(database *db.DB, ss models.SavedSearch) error {
	_, err := database.Write.Exec(
		`INSERT INTO saved_searches (name, query, sort, sort_order, seed) VALUES (?, ?, ?, ?, ?)`,
		ss.Name, ss.Query, ss.Sort, ss.Order, ss.Seed)
	return err
}

func DeleteSavedSearch(database *db.DB, id int64) error {
	_, err := database.Write.Exec(`DELETE FROM saved_searches WHERE id = ?`, id)
	return err
}

func SavedSearches(database *db.DB) ([]models.SavedSearch, error) {
	return db.QueryAll(database.Read, db.ScanRow(func(ss *models.SavedSearch) []any {
		return []any{&ss.ID, &ss.Name, &ss.Query, &ss.Sort, &ss.Order, &ss.Seed}
	}), `SELECT id, name, query, sort, sort_order, seed FROM saved_searches ORDER BY name`)
}
