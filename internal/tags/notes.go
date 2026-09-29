package tags

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	MaxTagNoteLen = 10000
	MaxTagLinks   = 100
	MaxTagLinkLen = 2048
)

type TagNote struct {
	Body  string
	Links []string
}

func (s *Service) TagNote(id int64) (TagNote, error) {
	var body, links string
	err := s.db.Read.QueryRow(`SELECT body, links FROM tag_notes WHERE tag_id = ?`, id).Scan(&body, &links)
	if errors.Is(err, sql.ErrNoRows) {
		return TagNote{}, nil
	}
	if err != nil {
		return TagNote{}, err
	}
	return TagNote{Body: body, Links: splitLinks(links)}, nil
}

// SetTagNote replaces the note and links; both empty drops the row.
func (s *Service) SetTagNote(id int64, body string, links []string) error {
	return s.inWriteTx(func(tx *sql.Tx) error {
		var isAlias int
		switch err := tx.QueryRow(`SELECT is_alias FROM tags WHERE id = ?`, id).Scan(&isAlias); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrTagNotFound
		case err != nil:
			return err
		case isAlias == 1:
			return ErrAliasNote
		}
		return writeTagNoteTx(tx, id, body, links)
	})
}

func writeTagNoteTx(tx *sql.Tx, id int64, body string, links []string) error {
	if body == "" && len(links) == 0 {
		_, err := tx.Exec(`DELETE FROM tag_notes WHERE tag_id = ?`, id)
		return err
	}
	_, err := tx.Exec(
		`INSERT INTO tag_notes (tag_id, body, links) VALUES (?, ?, ?)
		 ON CONFLICT(tag_id) DO UPDATE SET body = excluded.body, links = excluded.links`,
		id, body, strings.Join(links, "\n"))
	return err
}

func ParseTagLinks(raw string) ([]string, error) {
	links := tidyLinks(strings.Split(raw, "\n"))
	if len(links) > MaxTagLinks {
		return nil, fmt.Errorf("too many links (max %d)", MaxTagLinks)
	}
	for _, l := range links {
		if utf8.RuneCountInString(strings.TrimPrefix(l, "-")) > MaxTagLinkLen {
			return nil, fmt.Errorf("link too long (max %d chars)", MaxTagLinkLen)
		}
	}
	return links, nil
}

// A link and its dead form count as one; the first kept wins.
func tidyLinks(lines []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		dead := strings.HasPrefix(line, "-")
		target := strings.TrimSpace(strings.TrimPrefix(line, "-"))
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		if dead {
			target = "-" + target
		}
		out = append(out, target)
	}
	return out
}

func splitLinks(joined string) []string {
	if joined == "" {
		return nil
	}
	return strings.Split(joined, "\n")
}

// A merge cannot be undone, so the canonical keeps both notes rather than
// dropping the alias's.
func moveTagNoteTx(tx *sql.Tx, aliasID, canonicalID int64) error {
	var name, body, links string
	err := tx.QueryRow(
		`SELECT t.name, n.body, n.links FROM tag_notes n JOIN tags t ON t.id = n.tag_id WHERE n.tag_id = ?`,
		aliasID,
	).Scan(&name, &body, &links)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var canonBody, canonLinks string
	if err := tx.QueryRow(`SELECT body, links FROM tag_notes WHERE tag_id = ?`, canonicalID).
		Scan(&canonBody, &canonLinks); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	switch {
	case canonBody == "":
		canonBody = body
	case body != "" && body != canonBody:
		canonBody += "\n\nFrom " + name + ":\n" + body
	}
	merged := tidyLinks(append(splitLinks(canonLinks), splitLinks(links)...))
	if err := writeTagNoteTx(tx, canonicalID, canonBody, merged); err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM tag_notes WHERE tag_id = ?`, aliasID)
	return err
}
