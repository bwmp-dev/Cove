package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

func automaticScanOrigin(origin string) bool {
	return origin == "post_deploy" || origin == "scheduled"
}

func scanHasFindings(images []worker.ScannedImage) bool {
	for _, image := range images {
		for _, count := range image.Counts {
			if count > 0 {
				return true
			}
		}
	}
	return false
}

func recordedScanOutcome(operation worker.Operation, state string) string {
	if state != "succeeded" || operation.ImageScan == nil || !operation.ImageScan.Complete(operation.ImageScan.CheckedAt) {
		return "image.scan_unknown"
	}
	if scanHasFindings(operation.ImageScan.Images) {
		return "image.vulnerabilities"
	}
	return "image.scan_completed"
}

func sameStoredScanFindings(a, b *worker.ImageScan) bool {
	type key struct{ Image, Config, ID, Package, Installed string }
	keys := func(report *worker.ImageScan) map[key]bool {
		result := map[key]bool{}
		if report != nil {
			for _, image := range report.Images {
				for _, finding := range image.Findings {
					result[key{image.ImageID, image.ConfigDigest, finding.ID, finding.Package, finding.Installed}] = true
				}
			}
		}
		return result
	}
	left, right := keys(a), keys(b)
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if !right[value] {
			return false
		}
	}
	return true
}

func (s *Server) scanNotificationScope(payload []byte, report *worker.ImageScan) string {
	var request worker.Request
	if s.unsealJSON(payload, &request) == nil && request.AutomaticScan != nil && worker.ValidateRequest(request) == nil {
		data, _ := json.Marshal(request.AutomaticScan.Images)
		return "requested:" + string(data)
	}
	if report == nil || len(report.Images) == 0 {
		return ""
	}
	type image struct{ Service, ID, Config, Identity string }
	images := make([]string, 0, len(report.Images))
	for _, item := range report.Images {
		if item.ImageID == "" || item.ConfigDigest == "" {
			return ""
		}
		data, _ := json.Marshal(image{item.Service, item.ImageID, item.ConfigDigest, item.Identity})
		images = append(images, string(data))
	}
	sort.Strings(images)
	data, _ := json.Marshal(images)
	return "reported:" + string(data)
}

func (s *Server) automaticScanNotificationImages(ctx context.Context, id, outcome string, report *worker.ImageScan) ([]worker.ScannedImage, bool, error) {
	if err := s.scanBaselineReady(ctx, id); err != nil {
		return nil, false, err
	}
	var payload, previousPayload, data []byte
	var state string
	err := s.DB.QueryRow(ctx, `SELECT current.payload,p.payload,p.result,p.state FROM operations current JOIN LATERAL (
 SELECT payload,result,state FROM operations p WHERE p.workload_id=current.workload_id AND p.action='image_scan'
 AND p.state IN ('succeeded','failed') AND p.result->>'scan_origin' IN ('post_deploy','scheduled')
 AND (p.created_at,p.id)<(current.created_at,current.id) ORDER BY p.created_at DESC,p.id DESC LIMIT 1
 ) p ON true WHERE current.id=$1`, id).Scan(&payload, &previousPayload, &data, &state)
	previousExists := !errors.Is(err, pgx.ErrNoRows)
	if err != nil && previousExists {
		return nil, false, err
	}
	var previous worker.Operation
	if previousExists && (len(data) > 2*1024*1024 || json.Unmarshal(data, &previous) != nil) {
		return nil, false, nil
	}
	scope := s.scanNotificationScope(payload, report)
	sameScope := previousExists && scope != "" && scope == s.scanNotificationScope(previousPayload, previous.ImageScan)
	sameOutcome := sameScope && outcome == recordedScanOutcome(previous, state)
	if sameOutcome && sameStoredScanFindings(report, previous.ImageScan) {
		return nil, false, nil
	}
	if outcome != "image.scan_completed" {
		findings, err := s.newScanFindings(ctx, id, report)
		if err != nil {
			return nil, false, err
		}
		if len(findings) > 0 {
			counts := map[string]int{}
			for _, finding := range findings {
				counts[finding.Severity]++
			}
			return []worker.ScannedImage{{Counts: counts, Findings: findings}}, true, nil
		}
		if outcome == "image.vulnerabilities" {
			return nil, false, nil
		}
	}
	return report.Images, !sameOutcome, nil
}

func (s *Server) scanBaselineReady(ctx context.Context, id string) error {
	var pending bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM automatic_image_scans a JOIN operations o ON o.workload_id=a.workload_id WHERE o.id=$1 AND a.created_at<o.created_at AND a.id<>o.id)`, id).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return errScanBaselinePending
	}
	return nil
}

func currentAutomaticScanReport(report *worker.ImageScan, now time.Time) bool {
	return !report.CheckedAt.IsZero() && !report.CheckedAt.After(now) && (report.State != "complete" || report.Database.Fresh(now))
}
