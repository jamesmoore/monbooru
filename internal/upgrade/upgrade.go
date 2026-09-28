// Package upgrade holds the "source serves a file we do not have"
// predicate, in Go and in SQL.
package upgrade

import "github.com/monbooru/monbooru/internal/models"

// Eligible presumes that a similarity match with no hash claim differs
// from the local file.
func Eligible(s models.ImageSource) bool {
	if s.URL == "" || s.UpgradeKept {
		return false
	}
	return s.MD5Match == "differ" || (s.Similarity > 0 && s.MD5Match == "")
}

// CandidateWhere is Eligible in SQL. idx_image_sources_upgradable repeats
// it; keep them in step or the planner stops using the index.
func CandidateWhere(alias string) string {
	a := alias + "."
	return a + "url <> '' AND " + a + "upgrade_kept = 0 AND (" +
		a + "md5_match = 'differ' OR (" + a + "similarity > 0 AND " + a + "md5_match = ''))"
}
