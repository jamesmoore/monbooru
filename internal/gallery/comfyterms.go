package gallery

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/metadata"
)

// WriteComfyTerms must run in every transaction that writes a
// comfyui_metadata row, or the terms index lags the workflow.
func WriteComfyTerms(ctx context.Context, tx *sql.Tx, imageID int64, rawWorkflow string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM comfyui_terms WHERE image_id = ?`, imageID); err != nil {
		return fmt.Errorf("clear comfyui_terms: %w", err)
	}
	for _, term := range metadata.WorkflowTerms(rawWorkflow) {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO comfyui_terms (image_id, term) VALUES (?, ?)`, imageID, term,
		); err != nil {
			return fmt.Errorf("insert comfyui_term: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE comfyui_metadata SET terms_version = ? WHERE image_id = ?`,
		metadata.ComfyTermsVersion, imageID,
	); err != nil {
		return fmt.Errorf("stamp terms_version: %w", err)
	}
	return nil
}

// The write lock is held through the parse: a longer hold traded against
// a commit per row.
const comfyTermsChunk = 50

func ComfyTermsPending(database *db.DB) (int, error) {
	var n int
	err := database.Read.QueryRow(
		`SELECT COUNT(*) FROM comfyui_metadata WHERE terms_version < ?`, metadata.ComfyTermsVersion,
	).Scan(&n)
	return n, err
}

// BackfillComfyTerms reads each chunk inside its write transaction, so a
// concurrent re-extract cannot get the terms of this run's older read.
func BackfillComfyTerms(ctx context.Context, database *db.DB, progress func(processed, total int, message string)) (int, error) {
	total, err := ComfyTermsPending(database)
	if err != nil || total == 0 {
		return 0, err
	}
	indexed := 0
	for {
		if ctx.Err() != nil {
			return indexed, ctx.Err()
		}
		if progress != nil {
			progress(indexed, total, "")
		}
		n, err := backfillComfyTermsChunk(ctx, database)
		if err != nil {
			return indexed, err
		}
		if n == 0 {
			break
		}
		indexed += n
	}
	if progress != nil {
		progress(indexed, total, "Workflows…")
	}
	return indexed, nil
}

func backfillComfyTermsChunk(ctx context.Context, database *db.DB) (int, error) {
	tx, err := database.Write.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	type pending struct {
		id  int64
		raw string
	}
	rows, err := db.QueryAllContext(ctx, tx, func(r *sql.Rows) (pending, error) {
		var p pending
		err := r.Scan(&p.id, &p.raw)
		return p, err
	}, `SELECT image_id, COALESCE(raw_workflow, '') FROM comfyui_metadata WHERE terms_version < ? LIMIT ?`,
		metadata.ComfyTermsVersion, comfyTermsChunk)
	if err != nil {
		return 0, err
	}
	for _, p := range rows {
		if err := WriteComfyTerms(ctx, tx, p.id, p.raw); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(rows), nil
}
