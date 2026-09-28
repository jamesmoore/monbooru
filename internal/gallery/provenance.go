package gallery

import (
	"fmt"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

type MergeSummary struct {
	TagsAdded    int  `json:"tags_added"`
	TagsRetired  int  `json:"tags_retired"`
	RatingFilled bool `json:"rating_filled"`
	SourceAdded  bool `json:"source_added"`
}

func writeSourceProvenance(database *db.DB, imageID int64, source, postID, url, md5, parentURL string, post PostFile) error {
	if err := AddSourceMembership(database, imageID, source, postID, url); err != nil {
		return err
	}
	if err := setSourceMD5(database, imageID, source, postID, md5); err != nil {
		return err
	}
	if err := setSourcePostFile(database, imageID, source, postID, post); err != nil {
		return err
	}
	return setSourceParentURL(database, imageID, source, postID, parentURL)
}

// ApplySourceProvenance skips empty values; on failure the string names
// the step that failed.
func ApplySourceProvenance(database *db.DB, imageID int64, source, postID, commentary, translated, original string, notes []models.Annotation) (string, error) {
	if source == "" {
		return "", nil
	}
	if commentary != "" {
		if err := SetSourceCommentary(database, imageID, source, postID, commentary); err != nil {
			return "commentary", err
		}
	}
	if translated != "" {
		if err := SetSourceCommentaryTranslated(database, imageID, source, postID, translated); err != nil {
			return "the commentary translation", err
		}
	}
	if original != "" {
		if err := SetSourceOriginal(database, imageID, source, postID, original); err != nil {
			return "the original source", err
		}
	}
	if len(notes) > 0 {
		if err := ReplaceSourceAnnotations(database, imageID, source, postID, notes); err != nil {
			return "notes", err
		}
	}
	return "", nil
}

// ApplyCreateProvenance expects fields the caller has validated.
func ApplyCreateProvenance(database *db.DB, imageID int64, source, postID, url, md5, parentURL, collection, commentary, translated, original string, post PostFile, order *int) error {
	if source != "" || url != "" {
		if err := writeSourceProvenance(database, imageID, source, postID, url, md5, parentURL, post); err != nil {
			return err
		}
	}
	// Notes stay with the caller: a failed note write warns instead of
	// failing a create whose row landed.
	if _, err := ApplySourceProvenance(database, imageID, source, postID, commentary, translated, original, nil); err != nil {
		return err
	}
	if collection != "" {
		return SetHomeCollection(database, imageID, collection, order)
	}
	return nil
}

// MergeSource returns unresolvable tags as warnings. A booru origin takes
// the primary over from the url-less ptr row, so a lookup that hit both
// backends leads with the booru post.
func MergeSource(database *db.DB, tagSvc *tags.Service, imageID int64, source, postID, url, md5, parentURL string, post PostFile, rawTags []string) (MergeSummary, []string, error) {
	var sum MergeSummary
	if source != "" || url != "" {
		if err := writeSourceProvenance(database, imageID, source, postID, url, md5, parentURL, post); err != nil {
			return sum, nil, err
		}
		if source != "" && !strings.EqualFold(source, "ptr") {
			var primary string
			if err := database.Read.QueryRow(`SELECT source FROM images WHERE id = ?`, imageID).Scan(&primary); err == nil &&
				strings.EqualFold(strings.TrimSpace(primary), "ptr") {
				if err := MakeSourcePrimary(database, imageID, source, postID); err != nil {
					return sum, nil, err
				}
			}
		}
		sum.SourceAdded = true
	}
	var warnings []string
	if source != "" && len(rawTags) > 0 {
		tagIDs, warns := ResolveTagNames(database, tagSvc, rawTags, source)
		warnings = warns
		// Every post of a site shares the site's tag slice, so the
		// reconcile runs only for a site's sole origin; beside a sibling
		// post the merge is add-only.
		var origins int
		if err := database.Read.QueryRow(
			`SELECT COUNT(*) FROM image_sources WHERE image_id = ? AND site = ?`, imageID, source,
		).Scan(&origins); err != nil {
			return sum, warnings, err
		}
		r, err := tagSvc.SyncSourceTags(imageID, tagIDs, source, origins <= 1)
		if err != nil {
			return sum, warnings, err
		}
		sum.TagsAdded, sum.TagsRetired, sum.RatingFilled = r.Added, r.Retired, r.RatingFilled
	}
	return sum, warnings, nil
}

func ApplyPTRTags(database *db.DB, tagSvc *tags.Service, imageID int64, tagNames []string) error {
	_, _, err := MergeSource(database, tagSvc, imageID, "ptr", "", "", "", "", PostFile{}, tagNames)
	return err
}

func ResolveTagNames(database *db.DB, tagSvc *tags.Service, rawTags []string, origin string) ([]int64, []string) {
	var warnings []string
	tagIDs := make([]int64, 0, len(rawTags))
	for _, tagName := range rawTags {
		catID, bareName, err := resolveCategoryTag(database, tagName)
		if err != nil {
			warnings = append(warnings, "tag "+tagName+": "+err.Error())
			continue
		}
		tag, err := tagSvc.GetOrCreateTagFrom(bareName, catID, origin)
		if err != nil {
			warnings = append(warnings, "tag "+tagName+": "+err.Error())
			continue
		}
		tagIDs = append(tagIDs, tag.ID)
	}
	return tagIDs, warnings
}

// An unknown prefix keeps the whole name in general, so names like
// "nier:automata" or ":3" round-trip.
func resolveCategoryTag(database *db.DB, input string) (int64, string, error) {
	input = strings.TrimSpace(input)
	if idx := strings.Index(input, ":"); idx > 0 {
		catID, ok, err := tags.CategoryIDByName(database, input[:idx])
		if err != nil {
			return 0, "", err
		}
		if ok {
			return catID, input[idx+1:], nil
		}
	}
	catID, ok, err := tags.CategoryIDByName(database, "general")
	if err != nil {
		return 0, "", err
	}
	if !ok {
		return 0, "", fmt.Errorf("unknown category %q", "general")
	}
	return catID, input, nil
}
