package control

import (
	"context"
	"cove/internal/worker"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s *Server) previewCIRegistryReferences(ctx context.Context, tx pgx.Tx, team, server string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT configuration FROM environment_previews WHERE team_id=$1 AND state<>'deleting' ORDER BY id LIMIT 1025`, team)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := map[string]bool{}
	count := 0
	for rows.Next() {
		count++
		if count > 1024 {
			return nil, errors.New("preview reference inventory exceeds its bound")
		}
		var raw []byte
		var snapshot previewSnapshot
		if rows.Scan(&raw) != nil || s.unsealJSON(raw, &snapshot) != nil {
			return nil, errors.New("preview reference inventory is incomplete")
		}
		if snapshot.CI == nil {
			continue
		}
		if validatePreviewCIImages(snapshot.CI.Images, snapshot.CI.Bindings) != nil || snapshot.CI.Identity != previewCIInputIdentity(snapshot.CI.Revision, snapshot.CI.Images) {
			return nil, errors.New("CI preview reference inventory is incomplete")
		}
		for id, binding := range snapshot.CI.Bindings {
			pins := snapshot.CI.Images[id]
			if worker.ValidatePreviewRegistryImages(pins, binding.Server) != nil {
				return nil, errors.New("CI preview reference inventory is incomplete")
			}
			if binding.Server != server {
				continue
			}
			for _, image := range pins {
				refs[strings.TrimPrefix(image, server+"/")] = true
			}
			if len(refs) > worker.MaxManagedRegistryReferences {
				return nil, errors.New("CI preview reference inventory exceeds its bound")
			}
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	result := make([]string, 0, len(refs))
	for ref := range refs {
		result = append(result, ref)
	}
	return result, nil
}
