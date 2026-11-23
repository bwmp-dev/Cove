package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

type uptimeCheck struct {
	version             int
	ID                  string     `json:"id"`
	Domain              string     `json:"domain"`
	Path                string     `json:"path"`
	Enabled             bool       `json:"enabled"`
	Interval            int        `json:"interval_seconds"`
	Timeout             int        `json:"timeout_seconds"`
	ExpectedMin         int        `json:"expected_min"`
	ExpectedMax         int        `json:"expected_max"`
	PublicLabel         string     `json:"public_label"`
	CheckedAt           *time.Time `json:"checked_at"`
	Up                  *bool      `json:"up"`
	Status              *int       `json:"status"`
	LatencyMS           *float64   `json:"latency_ms"`
	Error               string     `json:"error"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
}

func validUptimeCheck(c uptimeCheck) bool {
	u, err := url.ParseRequestURI(c.Path)
	return err == nil && strings.HasPrefix(c.Path, "/") && !strings.HasPrefix(c.Path, "//") && !strings.ContainsAny(c.Path, "?#") && u.Scheme == "" && u.Host == "" && u.RawQuery == "" && u.Fragment == "" && len(c.Path) <= 2048 && strings.IndexFunc(c.Path, unicode.IsControl) < 0 && c.Interval >= 60 && c.Interval <= 86400 && c.Timeout >= 1 && c.Timeout <= 10 && c.ExpectedMin >= 100 && c.ExpectedMax >= c.ExpectedMin && c.ExpectedMax <= 599 && len(c.PublicLabel) <= 120 && strings.IndexFunc(c.PublicLabel, unicode.IsControl) < 0
}

func (s *Server) uptimeChecks(w http.ResponseWriter, r *http.Request) {
	ctx, workload, team := r.Context(), r.PathValue("id"), actor(r).Team
	if r.Method == http.MethodGet {
		rows, err := s.DB.Query(ctx, `SELECT id,domain,path,enabled,interval_seconds,timeout_seconds,expected_min,expected_max,public_label,checked_at,last_up,last_status,latency_ms,last_error,consecutive_failures FROM route_checks WHERE workload_id=$1 AND team_id=$2 ORDER BY domain`, workload, team)
		if err != nil {
			http.Error(w, "uptime checks unavailable", 503)
			return
		}
		defer rows.Close()
		out := []uptimeCheck{}
		for rows.Next() {
			var c uptimeCheck
			if rows.Scan(&c.ID, &c.Domain, &c.Path, &c.Enabled, &c.Interval, &c.Timeout, &c.ExpectedMin, &c.ExpectedMax, &c.PublicLabel, &c.CheckedAt, &c.Up, &c.Status, &c.LatencyMS, &c.Error, &c.ConsecutiveFailures) != nil {
				http.Error(w, "uptime checks unavailable", 503)
				return
			}
			out = append(out, c)
		}
		if rows.Err() != nil {
			http.Error(w, "uptime checks unavailable", 503)
			return
		}
		respond(w, 200, out)
		return
	}
	if r.Method == http.MethodDelete {
		result, err := s.DB.Exec(ctx, `DELETE FROM route_checks WHERE id=$1 AND workload_id=$2 AND team_id=$3`, r.PathValue("check"), workload, team)
		if err != nil {
			http.Error(w, "could not delete uptime check", 503)
			return
		}
		if result.RowsAffected() == 0 {
			http.NotFound(w, r)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	var c uptimeCheck
	if !decode(w, r, &c) {
		return
	}
	c.PublicLabel = strings.TrimSpace(c.PublicLabel)
	if !validUptimeCheck(c) {
		http.Error(w, "use a route path without query or fragment, interval 60-86400 seconds, timeout 1-10 seconds and valid expected statuses", 400)
		return
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		http.Error(w, "uptime checks unavailable", 503)
		return
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1129272911)`); err != nil {
		http.Error(w, "uptime checks unavailable", 503)
		return
	}
	var configuration []byte
	var kind string
	err = tx.QueryRow(ctx, `SELECT configuration,kind FROM workloads WHERE id=$1 AND team_id=$2 FOR SHARE`, workload, team).Scan(&configuration, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "resource unavailable", 503)
		return
	}
	plain, err := unseal(s.Key, configuration)
	var config workloadInput
	if err != nil || json.Unmarshal(plain, &config) != nil {
		http.Error(w, "resource configuration unavailable", 503)
		return
	}
	found := false
	for _, route := range config.Routes {
		if route.Domain == c.Domain {
			found = true
		}
	}
	if kind != "service" || !found {
		http.Error(w, "select a configured service route", 400)
		return
	}
	var total, teamCount int
	if err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE team_id=$1) FROM route_checks`, team).Scan(&total, &teamCount); err != nil {
		http.Error(w, "uptime checks unavailable", 503)
		return
	}
	id := r.PathValue("check")
	if id != "" {
		var existing, oldDomain, oldPath string
		var oldMin, oldMax int
		if err = tx.QueryRow(ctx, `SELECT id,domain,path,expected_min,expected_max FROM route_checks WHERE id=$1 AND workload_id=$2 AND team_id=$3 FOR UPDATE`, id, workload, team).Scan(&existing, &oldDomain, &oldPath, &oldMin, &oldMax); errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		} else if err != nil {
			http.Error(w, "uptime check unavailable", 503)
			return
		}
		if c.Domain != oldDomain {
			http.Error(w, "a check's domain cannot change; create a check for the other route", 400)
			return
		}
		if oldPath != c.Path || oldMin != c.ExpectedMin || oldMax != c.ExpectedMax {
			if _, err = tx.Exec(ctx, `DELETE FROM route_check_results WHERE check_id=$1`, id); err != nil {
				http.Error(w, "check history unavailable", 503)
				return
			}
		}
	}
	if id == "" && (total >= 256 || teamCount >= 64) {
		http.Error(w, "uptime checks are limited to 64 per team and 256 per panel", 409)
		return
	}
	var budget float64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(sum(timeout_seconds::double precision/interval_seconds),0) FROM route_checks WHERE enabled AND id<>$1`, id).Scan(&budget); err != nil {
		http.Error(w, "uptime schedule unavailable", 503)
		return
	}
	if c.Enabled {
		budget += float64(c.Timeout) / float64(c.Interval)
	}
	if budget > 2 {
		http.Error(w, "uptime schedule exceeds the panel's bounded checking capacity; increase intervals or reduce timeouts", 409)
		return
	}
	if id == "" {
		id = resourceID()
	}
	_, err = tx.Exec(ctx, `INSERT INTO route_checks(id,workload_id,team_id,domain,path,enabled,interval_seconds,timeout_seconds,expected_min,expected_max,public_label) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(id) DO UPDATE SET domain=EXCLUDED.domain,path=EXCLUDED.path,enabled=EXCLUDED.enabled,interval_seconds=EXCLUDED.interval_seconds,timeout_seconds=EXCLUDED.timeout_seconds,expected_min=EXCLUDED.expected_min,expected_max=EXCLUDED.expected_max,public_label=EXCLUDED.public_label,next_check_at=now(),checked_at=NULL,last_up=NULL,last_status=NULL,latency_ms=NULL,last_error='',consecutive_failures=0,version=route_checks.version+1`, id, workload, team, c.Domain, c.Path, c.Enabled, c.Interval, c.Timeout, c.ExpectedMin, c.ExpectedMax, c.PublicLabel)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		http.Error(w, "could not save uptime check; each resource route can have one check", 409)
		return
	}
	status := 200
	if r.Method == http.MethodPost {
		status = 201
	}
	respond(w, status, map[string]string{"id": id})
}

func (s *Server) probeUptime(ctx context.Context, c uptimeCheck) (bool, int, float64, string) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.Timeout)*time.Second)
	defer cancel()
	parsed, err := url.ParseRequestURI(c.Path)
	if err != nil {
		return false, 0, 0, "Invalid check path"
	}
	endpoint := url.URL{Scheme: "https", Host: c.Domain, Path: parsed.Path, RawPath: parsed.RawPath}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return false, 0, 0, "Invalid check endpoint"
	}
	request.Header.Set("User-Agent", "Cove-Uptime")
	proxy, err := s.outboundConfig(ctx)
	if err != nil {
		return false, 0, 0, "Outbound configuration unavailable"
	}
	transport := publicWebhookTransport(proxy)
	defer transport.CloseIdleConnections()
	var rt http.RoundTripper = transport
	if s.NotificationTransport != nil {
		rt = s.NotificationTransport
	}
	client := &http.Client{Transport: rt, Timeout: time.Duration(c.Timeout) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start := time.Now()
	response, err := client.Do(request)
	elapsed := float64(time.Since(start)) / float64(time.Millisecond)
	if err != nil {
		return false, 0, elapsed, "HTTPS check failed"
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	up := response.StatusCode >= c.ExpectedMin && response.StatusCode <= c.ExpectedMax
	message := ""
	if !up {
		message = "Unexpected HTTP status"
	}
	return up, response.StatusCode, elapsed, message
}

func (s *Server) collectUptime(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `UPDATE route_checks SET next_check_at=now()+make_interval(secs=>interval_seconds) WHERE id IN (SELECT id FROM route_checks WHERE enabled AND next_check_at<=now() ORDER BY next_check_at,id LIMIT 16 FOR UPDATE SKIP LOCKED) RETURNING id,workload_id,team_id,domain,path,interval_seconds,timeout_seconds,expected_min,expected_max,version`)
	if err != nil {
		return err
	}
	type job struct {
		check          uptimeCheck
		workload, team string
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err = rows.Scan(&j.check.ID, &j.workload, &j.team, &j.check.Domain, &j.check.Path, &j.check.Interval, &j.check.Timeout, &j.check.ExpectedMin, &j.check.ExpectedMax, &j.check.version); err != nil {
			break
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	failures := make(chan error, len(jobs))
	slots := make(chan struct{}, 4)
	var group sync.WaitGroup
	for _, j := range jobs {
		group.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			var configuration []byte
			if err := s.DB.QueryRow(ctx, `SELECT configuration FROM workloads WHERE id=$1 AND team_id=$2`, j.workload, j.team).Scan(&configuration); err != nil {
				if !errors.Is(err, pgx.ErrNoRows) {
					failures <- err
				}
				return
			}
			plain, err := unseal(s.Key, configuration)
			var config workloadInput
			if err != nil || json.Unmarshal(plain, &config) != nil {
				failures <- errors.New("uptime resource configuration unavailable")
				return
			}
			configured := false
			for _, route := range config.Routes {
				if route.Domain == j.check.Domain {
					configured = true
				}
			}
			if !configured {
				_, err = s.DB.Exec(ctx, `UPDATE route_checks SET enabled=false,last_up=NULL,last_error='Route removed; recreate or edit this check before enabling it.' WHERE id=$1 AND version=$2`, j.check.ID, j.check.version)
				if err != nil {
					failures <- err
				}
				return
			}
			up, status, latency, message := s.probeUptime(ctx, j.check)
			tx, err := s.DB.Begin(ctx)
			if err != nil {
				failures <- err
				return
			}
			defer tx.Rollback(ctx)
			var consecutive int
			err = tx.QueryRow(ctx, `UPDATE route_checks SET checked_at=now(),last_up=$2,last_status=NULLIF($3,0),latency_ms=$4,last_error=$5,consecutive_failures=CASE WHEN $2 THEN 0 ELSE LEAST(1000000,consecutive_failures+1) END WHERE id=$1 AND enabled AND version=$6 RETURNING consecutive_failures`, j.check.ID, up, status, latency, message, j.check.version).Scan(&consecutive)
			if errors.Is(err, pgx.ErrNoRows) {
				return
			}
			if err != nil {
				failures <- err
				return
			}
			_, err = tx.Exec(ctx, `INSERT INTO route_check_results(check_id,checked_at,up,status,latency_ms) VALUES($1,now(),$2,$3,$4)`, j.check.ID, up, status, latency)
			if err == nil {
				err = tx.Commit(ctx)
			}
			if err != nil {
				failures <- err
				return
			}
			if err = s.evaluateUptimeRules(ctx, j.workload, j.check.Domain, consecutive); err != nil {
				failures <- err
			}
		})
	}
	group.Wait()
	close(failures)
	for err := range failures {
		return err
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM route_check_results WHERE id<=(SELECT max(id)-500000 FROM route_check_results) OR checked_at<now()-interval '30 days'`)
	return err
}

func (s *Server) evaluateUptimeRules(ctx context.Context, workload, domain string, consecutive int) error {
	rows, err := s.DB.Query(ctx, `SELECT r.id,r.team_id,r.name,r.threshold,w.manually_stopped FROM threshold_rules r JOIN workloads w ON w.id=r.workload_id AND w.team_id=r.team_id WHERE r.workload_id=$1 AND r.domain=$2 AND r.metric='uptime'`, workload, domain)
	if err != nil {
		return err
	}
	type rule struct {
		id, team, name string
		threshold      float64
		stopped        bool
	}
	var rules []rule
	for rows.Next() {
		var r rule
		if err = rows.Scan(&r.id, &r.team, &r.name, &r.threshold, &r.stopped); err != nil {
			break
		}
		rules = append(rules, r)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	for _, r := range rules {
		if err = s.recordHealthRouted(ctx, r.team, "threshold_"+r.id, workload, "Uptime check for "+domain+": "+r.name, !r.stopped && float64(consecutive) >= r.threshold, r.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) runUptimeChecks(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := s.collectUptime(ctx); err != nil && ctx.Err() == nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Server) statusPageSettings(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	var exists bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1 AND team_id=$2)`, id, actor(r).Team).Scan(&exists); err != nil {
		http.Error(w, "project unavailable", 503)
		return
	}
	if !exists {
		http.NotFound(w, r)
		return
	}
	type config struct {
		Enabled bool   `json:"enabled"`
		Title   string `json:"title"`
		Slug    string `json:"slug"`
	}
	c := config{}
	if r.Method == http.MethodPut {
		if !decode(w, r, &c) {
			return
		}
		c.Title = strings.TrimSpace(c.Title)
		if len(c.Title) > 120 || c.Enabled && c.Title == "" {
			http.Error(w, "a public status title is required and must not exceed 120 characters", 400)
			return
		}
		_, err := s.DB.Exec(ctx, `INSERT INTO project_status_pages(project_id,enabled,title,slug) SELECT id,$2,$3,$4 FROM projects WHERE id=$1 AND team_id=$5 ON CONFLICT(project_id) DO UPDATE SET enabled=EXCLUDED.enabled,title=EXCLUDED.title`, id, c.Enabled, c.Title, resourceID()+resourceID(), actor(r).Team)
		if err != nil {
			http.Error(w, "could not save status page", 503)
			return
		}
	}
	err := s.DB.QueryRow(ctx, `SELECT p.enabled,p.title,p.slug FROM project_status_pages p JOIN projects pr ON pr.id=p.project_id WHERE p.project_id=$1 AND pr.team_id=$2`, id, actor(r).Team).Scan(&c.Enabled, &c.Title, &c.Slug)
	if errors.Is(err, pgx.ErrNoRows) {
		respond(w, 200, config{})
		return
	}
	if err != nil {
		http.Error(w, "status page unavailable", 503)
		return
	}
	respond(w, 200, c)
}

func (s *Server) publicStatusPage(w http.ResponseWriter, r *http.Request) {
	address, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		http.Error(w, "invalid client address", 400)
		return
	}
	if !s.limitAttempts(w, r, "status:"+address, 120) {
		return
	}
	if len(r.PathValue("slug")) != 64 {
		http.NotFound(w, r)
		return
	}
	var project, title string
	err = s.DB.QueryRow(r.Context(), `SELECT project_id,title FROM project_status_pages WHERE slug=$1 AND enabled`, r.PathValue("slug")).Scan(&project, &title)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "status unavailable", 503)
		return
	}
	rows, err := s.DB.Query(r.Context(), `SELECT c.public_label,CASE WHEN c.checked_at IS NULL OR c.checked_at<now()-make_interval(secs=>c.interval_seconds*2+c.timeout_seconds) THEN NULL ELSE c.last_up END,c.checked_at,c.latency_ms,(SELECT count(*) FROM route_check_results r WHERE r.check_id=c.id AND r.checked_at>now()-interval '24 hours'),(SELECT avg(CASE WHEN r.up THEN 1.0 ELSE 0.0 END)::double precision FROM route_check_results r WHERE r.check_id=c.id AND r.checked_at>now()-interval '24 hours') FROM route_checks c JOIN workloads w ON w.id=c.workload_id WHERE w.project_id=$1 AND c.enabled AND c.public_label<>'' ORDER BY c.public_label`, project)
	if err != nil {
		http.Error(w, "status unavailable", 503)
		return
	}
	defer rows.Close()
	type item struct {
		Label     string     `json:"label"`
		Up        *bool      `json:"up"`
		CheckedAt *time.Time `json:"checked_at"`
		LatencyMS *float64   `json:"latency_ms"`
		Samples   int        `json:"observed_samples_24h"`
		UpRatio   *float64   `json:"observed_up_ratio_24h"`
	}
	out := []item{}
	for rows.Next() {
		var i item
		if rows.Scan(&i.Label, &i.Up, &i.CheckedAt, &i.LatencyMS, &i.Samples, &i.UpRatio) != nil {
			http.Error(w, "status unavailable", 503)
			return
		}
		out = append(out, i)
	}
	if rows.Err() != nil {
		http.Error(w, "status unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"title": title, "checks": out, "source": "Periodic HTTPS checks from the panel; missing or stale readings are unknown."})
}
