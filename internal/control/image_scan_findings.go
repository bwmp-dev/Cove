package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

var errScanBaselinePending = errors.New("an earlier automatic scan result is still settling")

type scanFindingIdentity struct {
	ID, Package, Installed string
}

func (s *Server) newScanFindings(ctx context.Context, id string, report *worker.ImageScan) ([]worker.ScanFinding, error) {
	var pending bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM automatic_image_scans a JOIN operations o ON o.workload_id=a.workload_id WHERE o.id=$1 AND a.created_at<o.created_at AND a.id<>o.id)`, id).Scan(&pending); err != nil {
		return nil, err
	}
	if pending {
		return nil, errScanBaselinePending
	}
	result := []worker.ScanFinding{}
	seen := map[string]bool{}
	for _, image := range report.Images {
		imageKey := image.ImageID + "/" + image.ConfigDigest
		if seen[imageKey] {
			continue
		}
		seen[imageKey] = true
		match, _ := json.Marshal([]map[string]string{{"image_id": image.ImageID, "config_digest": image.ConfigDigest}})
		var data []byte
		err := s.DB.QueryRow(ctx, `SELECT p.result FROM operations p JOIN operations current ON current.workload_id=p.workload_id
 WHERE current.id=$1 AND p.action='image_scan' AND p.state='succeeded'
 AND (p.created_at,p.id)<(current.created_at,current.id)
 AND p.result->'image_scan'->'images' @> $2::jsonb
 ORDER BY p.created_at DESC,p.id DESC LIMIT 1`, id, match).Scan(&data)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		previous := map[scanFindingIdentity]bool{}
		truncated := false
		if err == nil {
			var prior worker.Operation
			if len(data) > 2*1024*1024 || json.Unmarshal(data, &prior) != nil || prior.ImageScan == nil {
				continue
			}
			for _, earlier := range prior.ImageScan.Images {
				if earlier.ImageID != image.ImageID || earlier.ConfigDigest != image.ConfigDigest {
					continue
				}
				truncated = truncated || earlier.Truncated
				for _, finding := range earlier.Findings {
					previous[scanFindingIdentity{finding.ID, finding.Package, finding.Installed}] = true
				}
			}
		}
		for _, finding := range image.Findings {
			key := scanFindingIdentity{finding.ID, finding.Package, finding.Installed}
			if !previous[key] && !truncated {
				result = append(result, finding)
				previous[key] = true
			}
		}
	}
	return result, nil
}
