package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Server) expirePreview(ctx context.Context, id string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	p, err := s.loadPreviewRow(ctx, tx, id, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if p.State != "deleting" && !p.Pinned && p.LastActivity.Add(time.Duration(p.Snapshot.Recipe.TTLHours)*time.Hour).Before(time.Now()) {
		if err = s.requestPreviewDeletion(ctx, tx, id, "inactive for "+fmt.Sprint(p.Snapshot.Recipe.TTLHours)+" hours"); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
