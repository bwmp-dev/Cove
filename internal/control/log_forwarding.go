package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Server) readLogForwarder(ctx context.Context, id, team string) (logForwarder, error) {
	var data []byte
	var c logForwarder
	err := s.DB.QueryRow(ctx, `SELECT configuration FROM log_forwarders WHERE id=$1 AND team_id=$2`, id, team).Scan(&data)
	if err != nil {
		return c, err
	}
	plain, err := unseal(s.Key, data)
	if err == nil {
		err = json.Unmarshal(plain, &c)
	}
	return c, err
}

func (s *Server) logForwarders(w http.ResponseWriter, r *http.Request) {
	ctx, team, id := r.Context(), actor(r).Team, r.PathValue("id")
	if r.Method == http.MethodGet {
		rows, err := s.DB.Query(ctx, `SELECT id,configuration,last_success,last_error,sent_entries,dropped_entries,(SELECT count(*) FROM log_forward_deliveries d WHERE d.forwarder_id=f.id) FROM log_forwarders f WHERE team_id=$1 ORDER BY name,id`, team)
		if err != nil {
			http.Error(w, "log forwarders unavailable", 503)
			return
		}
		defer rows.Close()
		type item struct {
			ID          string     `json:"id"`
			Name        string     `json:"name"`
			Kind        string     `json:"kind"`
			Enabled     bool       `json:"enabled"`
			EndpointSet bool       `json:"endpoint_set"`
			TokenSet    bool       `json:"token_set"`
			UsernameSet bool       `json:"username_set"`
			LastSuccess *time.Time `json:"last_success"`
			LastError   string     `json:"last_error"`
			Sent        int64      `json:"sent_entries"`
			Dropped     int64      `json:"dropped_entries"`
			Pending     int64      `json:"pending_batches"`
		}
		out := []item{}
		for rows.Next() {
			var i item
			var data []byte
			if rows.Scan(&i.ID, &data, &i.LastSuccess, &i.LastError, &i.Sent, &i.Dropped, &i.Pending) != nil {
				http.Error(w, "log forwarders unavailable", 503)
				return
			}
			plain, err := unseal(s.Key, data)
			var c logForwarder
			if err != nil || json.Unmarshal(plain, &c) != nil {
				http.Error(w, "log forwarder configuration unavailable", 503)
				return
			}
			i.Name = c.Name
			i.Kind = c.Kind
			i.Enabled = c.Enabled
			i.EndpointSet = c.Endpoint != ""
			i.TokenSet = c.Token != ""
			i.UsernameSet = c.Username != ""
			out = append(out, i)
		}
		if rows.Err() != nil {
			http.Error(w, "log forwarders unavailable", 503)
			return
		}
		respond(w, 200, out)
		return
	}
	if r.Method == http.MethodDelete {
		result, err := s.DB.Exec(ctx, `DELETE FROM log_forwarders WHERE id=$1 AND team_id=$2`, id, team)
		if err != nil {
			http.Error(w, "could not delete log forwarder", 503)
			return
		}
		if result.RowsAffected() == 0 {
			http.NotFound(w, r)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	if strings.HasSuffix(r.URL.Path, "/test") {
		c, err := s.readLogForwarder(ctx, id, team)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "log forwarder unavailable", 503)
			return
		}
		if err = s.sendLogBatch(ctx, c, []forwardedLog{{Workload: "test", Container: "cove", Timestamp: time.Now().UTC(), Message: "Cove log forwarding test"}}); err != nil {
			http.Error(w, "log forwarding test failed; verify the endpoint, credentials and public TLS reachability", 502)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	var c logForwarder
	if !decode(w, r, &c) {
		return
	}
	c.Name = strings.TrimSpace(c.Name)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		http.Error(w, "log forwarders unavailable", 503)
		return
	}
	defer tx.Rollback(ctx)
	var teamLock string
	if err = tx.QueryRow(ctx, `SELECT id FROM teams WHERE id=$1 FOR UPDATE`, team).Scan(&teamLock); err != nil {
		http.Error(w, "team unavailable", 503)
		return
	}
	if id != "" {
		var data []byte
		err = tx.QueryRow(ctx, `SELECT configuration FROM log_forwarders WHERE id=$1 AND team_id=$2 FOR UPDATE`, id, team).Scan(&data)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "log forwarder unavailable", 503)
			return
		}
		plain, err := unseal(s.Key, data)
		var old logForwarder
		if err != nil || json.Unmarshal(plain, &old) != nil {
			http.Error(w, "log forwarder unavailable", 503)
			return
		}
		if old.Kind == c.Kind {
			if c.Endpoint == "" {
				c.Endpoint = old.Endpoint
			}
			if c.Token == "" {
				c.Token = old.Token
			}
			if c.Username == "" {
				c.Username = old.Username
			}
		}
	} else {
		var count int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM log_forwarders WHERE team_id=$1`, team).Scan(&count); err != nil {
			http.Error(w, "log forwarders unavailable", 503)
			return
		}
		if count >= 8 {
			http.Error(w, "a team may have at most eight log forwarders", 409)
			return
		}
		id = resourceID()
	}
	if !validLogForwarder(c) {
		http.Error(w, "select a name, supported receiver and valid HTTPS or TLS syslog endpoint", 400)
		return
	}
	data, err := json.Marshal(c)
	if err == nil {
		data, err = seal(s.Key, data)
	}
	if err != nil {
		http.Error(w, "could not seal log forwarder", 503)
		return
	}
	_, err = tx.Exec(ctx, `INSERT INTO log_forwarders(id,team_id,name,enabled,configuration) VALUES($1,$2,$3,$4,$5) ON CONFLICT(id) DO UPDATE SET name=EXCLUDED.name,enabled=EXCLUDED.enabled,configuration=EXCLUDED.configuration`, id, team, c.Name, c.Enabled, data)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		http.Error(w, "could not save log forwarder; names must be unique", 409)
		return
	}
	status := 200
	if r.Method == http.MethodPost {
		status = 201
	}
	respond(w, status, map[string]string{"id": id})
}

func (s *Server) enqueueLogForwards(ctx context.Context, tx pgx.Tx, workload string, entries []forwardedLog) error {
	if len(entries) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT f.id FROM log_forwarders f JOIN workloads w ON w.team_id=f.team_id WHERE w.id=$1 AND f.enabled ORDER BY f.id FOR KEY SHARE OF f`, workload)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err != nil {
		return err
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1129272910)`); err != nil {
		return err
	}
	var count int
	var bytes int64
	if err = tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(octet_length(payload)+256),0) FROM log_forward_deliveries`).Scan(&count, &bytes); err != nil {
		return err
	}
	for len(entries) > 0 {
		end, size := 0, 2
		for end < len(entries) && end < 100 {
			data, _ := json.Marshal(entries[end])
			entrySize := len(data)
			if end > 0 {
				entrySize++
			}
			if size+entrySize > 256*1024 && end > 0 {
				break
			}
			size += entrySize
			end++
		}
		batch := entries[:end]
		entries = entries[end:]
		data, err := json.Marshal(batch)
		if err != nil {
			return err
		}
		data, err = seal(s.Key, data)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if count >= 2000 || bytes+int64(len(data))+256 > 64*1024*1024 {
				if _, err = tx.Exec(ctx, `UPDATE log_forwarders SET dropped_entries=dropped_entries+$2,last_error='Forwarding queue limit reached; new entries were dropped.' WHERE id=$1`, id, len(batch)); err != nil {
					return err
				}
				continue
			}
			if _, err = tx.Exec(ctx, `INSERT INTO log_forward_deliveries(id,forwarder_id,payload,entries) VALUES($1,$2,$3,$4)`, resourceID(), id, data, len(batch)); err != nil {
				return err
			}
			count++
			bytes += int64(len(data)) + 256
		}
	}
	return nil
}

func (s *Server) dispatchLogForwards(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `WITH expired AS (DELETE FROM log_forward_deliveries WHERE created_at<now()-interval '24 hours' RETURNING forwarder_id,entries), totals AS (SELECT forwarder_id,sum(entries) AS n FROM expired GROUP BY forwarder_id) UPDATE log_forwarders f SET dropped_entries=f.dropped_entries+t.n,last_error='Queued logs expired after 24 hours.' FROM totals t WHERE f.id=t.forwarder_id`)
	if err != nil {
		return err
	}
	rows, err := s.DB.Query(ctx, `UPDATE log_forward_deliveries SET attempts=attempts+1,next_attempt=now()+interval '2 minutes' WHERE id IN (SELECT d.id FROM log_forward_deliveries d WHERE next_attempt<=now() AND NOT EXISTS(SELECT 1 FROM log_forward_deliveries older WHERE older.forwarder_id=d.forwarder_id AND (older.created_at,older.id)<(d.created_at,d.id)) ORDER BY created_at,id LIMIT 4 FOR UPDATE SKIP LOCKED) RETURNING id,forwarder_id,payload,attempts,entries`)
	if err != nil {
		return err
	}
	type item struct {
		id, forwarder     string
		payload           []byte
		attempts, entries int
	}
	var jobs []item
	for rows.Next() {
		var j item
		if err = rows.Scan(&j.id, &j.forwarder, &j.payload, &j.attempts, &j.entries); err != nil {
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
	for _, j := range jobs {
		var data []byte
		err = s.DB.QueryRow(ctx, `SELECT configuration FROM log_forwarders WHERE id=$1`, j.forwarder).Scan(&data)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		plain, err := unseal(s.Key, data)
		var c logForwarder
		if err == nil {
			err = json.Unmarshal(plain, &c)
		}
		var logs []forwardedLog
		if err == nil {
			plain, err = unseal(s.Key, j.payload)
		}
		if err == nil {
			err = json.Unmarshal(plain, &logs)
		}
		if err == nil && c.Enabled {
			err = s.sendLogBatch(ctx, c, logs)
		}
		if err != nil && j.attempts < 6 {
			_, err = s.DB.Exec(ctx, `UPDATE log_forwarders SET last_error='Log delivery failed; the bounded queue will retry.' WHERE id=$1`, j.forwarder)
			if err != nil {
				return err
			}
			if _, err = s.DB.Exec(ctx, `UPDATE log_forward_deliveries SET next_attempt=now()+make_interval(secs=>LEAST(3600,30*power(2,attempts)::integer)) WHERE id=$1`, j.id); err != nil {
				return err
			}
			continue
		}
		tx, txErr := s.DB.Begin(ctx)
		if txErr != nil {
			return txErr
		}
		if err != nil || !c.Enabled {
			_, txErr = tx.Exec(ctx, `UPDATE log_forwarders SET dropped_entries=dropped_entries+$2,last_error='Log delivery exhausted retries or the forwarder was disabled.' WHERE id=$1`, j.forwarder, j.entries)
		} else {
			_, txErr = tx.Exec(ctx, `UPDATE log_forwarders SET sent_entries=sent_entries+$2,last_success=now(),last_error='' WHERE id=$1`, j.forwarder, j.entries)
		}
		if txErr == nil {
			_, txErr = tx.Exec(ctx, `DELETE FROM log_forward_deliveries WHERE id=$1`, j.id)
		}
		if txErr == nil {
			txErr = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
		if txErr != nil {
			return txErr
		}
	}
	return nil
}

func (s *Server) runLogForwards(ctx context.Context) error {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		if err := s.dispatchLogForwards(ctx); err != nil && ctx.Err() == nil {
			logPeriodicError(ctx, "dispatchLogForwards", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
