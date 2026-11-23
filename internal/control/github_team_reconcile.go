package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

var errGithubDirectoryConflict = errors.New("GitHub directory revision or refresh conflict")
var errGithubDirectoryProvider = errors.New("GitHub directory provider unavailable")

type githubDirectoryReport struct {
	Version   int64                   `json:"version"`
	Hash      string                  `json:"roster_hash"`
	Changes   []directoryMemberChange `json:"changes"`
	Unmatched int                     `json:"unmatched_github_users"`
	Applied   bool                    `json:"applied"`
}

func (s *Server) githubDirectoryToken(ctx context.Context, source githubTeamSource) (string, error) {
	if source.Config.Mode == "oauth" {
		var valid bool
		if err := s.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND github_id=$2 AND is_admin AND NOT disabled)", source.Config.OAuthUser, source.Config.OAuthGitHubID).Scan(&valid); err != nil {
			return "", err
		}
		if !valid || source.Config.OAuthToken == "" {
			return "", errGithubDirectoryProvider
		}
		return source.Config.OAuthToken, nil
	}
	jwt, err := s.appToken(ctx)
	if err != nil {
		return "", errors.Join(errGithubDirectoryProvider, err)
	}
	var installation appInstallation
	if err = s.appAPI(ctx, "GET", fmt.Sprintf("/app/installations/%d", source.Config.Installation), jwt, nil, &installation); err != nil || installation.SuspendedAt != nil || installation.ID != source.Config.Installation || !strings.EqualFold(installation.Account.Login, source.Config.Account) {
		return "", errGithubDirectoryProvider
	}
	var access struct {
		Token string `json:"token"`
	}
	if err = s.appAPI(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", source.Config.Installation), jwt, map[string]any{"permissions": map[string]string{"members": "read"}}, &access); err != nil || access.Token == "" {
		return "", errGithubDirectoryProvider
	}
	return access.Token, nil
}

func (s *Server) githubDirectoryRoster(ctx context.Context, source githubTeamSource, token string) (map[string][]string, error) {
	groups := map[string][]string{}
	for _, mapping := range source.Mappings {
		if _, found := groups[mapping.Group]; found {
			continue
		}
		members := map[string]bool{}
		for page := 1; ; page++ {
			if page > 100 {
				return nil, errGithubDirectoryProvider
			}
			var batch []githubAccount
			path := fmt.Sprintf("/orgs/%s/teams/%s/members?per_page=100&page=%d", url.PathEscape(source.Config.Account), url.PathEscape(mapping.Group), page)
			if err := s.appAPI(ctx, "GET", path, token, nil, &batch); err != nil {
				return nil, errors.Join(errGithubDirectoryProvider, err)
			}
			if batch == nil || len(batch) > 100 {
				return nil, errGithubDirectoryProvider
			}
			for _, member := range batch {
				if member.ID <= 0 || member.Login == "" {
					return nil, errGithubDirectoryProvider
				}
				members[strconv.FormatInt(member.ID, 10)] = true
			}
			if len(batch) < 100 {
				break
			}
		}
		groups[mapping.Group] = slices.Sorted(slices.Values(mapKeys(members)))
	}
	return groups, nil
}

func mapKeys(in map[string]bool) []string {
	out := make([]string, 0, len(in))
	for key := range in {
		out = append(out, key)
	}
	return out
}

func (s *Server) reconcileGithubTeamSource(ctx context.Context, id, by, user string, apply bool, expected int64, expectedHash string) (report githubDirectoryReport, result error) {
	read, err := s.DB.Begin(ctx)
	if err != nil {
		return report, err
	}
	source, err := loadGithubTeamSource(ctx, read, s.Key, id)
	read.Rollback(ctx)
	if err != nil {
		return report, err
	}
	if expected > 0 && source.Version != expected {
		return report, errGithubDirectoryConflict
	}
	if apply {
		lease, err := s.DB.Exec(ctx, "UPDATE github_team_sync_sources SET refresh_until=now()+interval '2 minutes' WHERE id=$1 AND version=$2 AND refresh_until<=now()", id, source.Version)
		if err != nil {
			return report, err
		}
		if lease.RowsAffected() == 0 {
			return report, errGithubDirectoryConflict
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			message := ""
			if result != nil {
				message = "Synchronization failed; existing membership was retained. Check the GitHub connection, organization and Members permission."
			}
			if _, err := s.DB.Exec(cleanup, "UPDATE github_team_sync_sources SET refresh_until=now(),checked_at=now(),last_error=$3,next_check_at=CASE WHEN $4='' THEN now()+interval '15 minutes' ELSE next_check_at END WHERE id=$1 AND version=$2", id, source.Version, message, user); err != nil {
				slog.Error("GitHub directory refresh state could not be persisted")
			}
		}()
	}
	token, err := s.githubDirectoryToken(ctx, source)
	if err != nil {
		return report, err
	}
	roster, err := s.githubDirectoryRoster(ctx, source, token)
	if err != nil {
		return report, err
	}
	tx, err := s.identityTransaction(ctx)
	if err != nil {
		return report, err
	}
	defer tx.Rollback(ctx)
	var current int64
	if err = tx.QueryRow(ctx, "SELECT version FROM github_team_sync_sources WHERE id=$1 FOR UPDATE", id).Scan(&current); err != nil || current != source.Version {
		return report, errGithubDirectoryConflict
	}
	if source.Config.Mode == "oauth" {
		var valid bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND github_id=$2 AND is_admin AND NOT disabled)", source.Config.OAuthUser, source.Config.OAuthGitHubID).Scan(&valid); err != nil {
			return report, err
		}
		if !valid {
			return report, errGithubDirectoryProvider
		}
	} else {
		var account string
		if err = tx.QueryRow(ctx, "SELECT account FROM github_installations WHERE id=$1 FOR SHARE", source.Config.Installation).Scan(&account); err != nil || !strings.EqualFold(account, source.Config.Account) {
			return report, errGithubDirectoryProvider
		}
	}
	all := map[string]bool{}
	for _, members := range roster {
		for _, member := range members {
			all[member] = true
		}
	}
	people := map[string]string{}
	rows, err := tx.Query(ctx, "SELECT id,github_id FROM users WHERE github_id=ANY($1) AND NOT disabled AND ($2='' OR id=$2) ORDER BY id FOR SHARE", mapKeys(all), user)
	if err != nil {
		return report, err
	}
	for rows.Next() {
		var local, remote string
		if err = rows.Scan(&local, &remote); err != nil {
			break
		}
		people[remote] = local
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		return report, errors.New("directory users unavailable")
	}
	members := map[string][]string{}
	for group, ids := range roster {
		for _, id := range ids {
			if local := people[id]; local != "" {
				members[group] = append(members[group], local)
			}
		}
	}
	claims, err := directoryMappingClaims(ctx, tx, source.Mappings, members)
	if err != nil {
		return report, err
	}
	raw, err := json.Marshal(map[string]any{"version": source.Version, "roster": roster, "claims": claims})
	if err != nil {
		return report, err
	}
	digest := sha256.Sum256(raw)
	report.Hash, report.Version = hex.EncodeToString(digest[:]), source.Version
	if expectedHash != "" && expectedHash != report.Hash {
		return report, errGithubDirectoryConflict
	}
	report.Unmatched = len(all) - len(people)
	report.Changes, err = syncDirectoryMemberships(ctx, tx, "github", id, user, claims, by)
	if err != nil {
		return report, err
	}
	for i := range report.Changes {
		change := &report.Changes[i]
		if err = tx.QueryRow(ctx, "SELECT name,COALESCE(email,'') FROM users WHERE id=$1", change.User).Scan(&change.Name, &change.Email); err != nil {
			return report, err
		}
	}
	if apply {
		if _, err = tx.Exec(ctx, "UPDATE github_team_sync_sources SET applied_at=now() WHERE id=$1", id); err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			return report, err
		}
		report.Applied = true
	}
	return report, nil
}

func (s *Server) storeGithubTeamOAuth(ctx context.Context, user, remote, token string) (bool, error) {
	tx, err := s.identityTransaction(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var owner bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND github_id=$2 AND is_admin AND NOT disabled)", user, remote).Scan(&owner); err != nil {
		return false, err
	}
	if !owner {
		return false, nil
	}
	rows, err := tx.Query(ctx, "SELECT id FROM github_team_sync_sources ORDER BY id")
	if err != nil {
		return false, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err != nil || rows.Err() != nil {
		return false, errors.New("directory settings unavailable")
	}
	stored := false
	for _, id := range ids {
		source, e := loadGithubTeamSource(ctx, tx, s.Key, id)
		if e != nil {
			return false, e
		}
		if source.Config.Mode != "oauth" || source.Config.OAuthUser != user || source.Config.OAuthGitHubID != remote {
			continue
		}
		source.Config.OAuthToken = token
		raw, e := json.Marshal(source.Config)
		if e == nil {
			raw, e = seal(s.Key, raw)
		}
		if e != nil {
			return false, e
		}
		if _, err = tx.Exec(ctx, "UPDATE github_team_sync_sources SET configuration=$2,version=version+1,next_check_at=now() WHERE id=$1", id, raw); err != nil {
			return false, err
		}
		stored = true
	}
	err = tx.Commit(ctx)
	return stored && err == nil, err
}

func (s *Server) githubTeamSignIn(ctx context.Context, user string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var linked bool
	if err := s.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND github_id IS NOT NULL AND NOT disabled)", user).Scan(&linked); err != nil {
		slog.Error("GitHub sign-in identity unavailable")
		return
	}
	if !linked {
		return
	}
	ids, err := s.githubTeamSourceIDs(ctx, false)
	if err != nil {
		slog.Error("GitHub sign-in membership refresh unavailable")
		return
	}
	for _, id := range ids {
		if _, err = s.reconcileGithubTeamSource(ctx, id, "GitHub sign-in", user, true, 0, ""); err != nil && !errors.Is(err, errGithubDirectoryConflict) {
			slog.Warn("GitHub sign-in membership retained after refresh failure")
		}
	}
}

func (s *Server) githubDirectoryNeedsOrgScope(ctx context.Context, user string) (bool, error) {
	if user == "" {
		return false, nil
	}
	rows, err := s.DB.Query(ctx, "SELECT configuration FROM github_team_sync_sources")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var config githubTeamConfiguration
		if err = rows.Scan(&raw); err != nil {
			return false, err
		}
		raw, err = unseal(s.Key, raw)
		if err != nil {
			return false, err
		}
		if err = json.Unmarshal(raw, &config); err != nil {
			return false, err
		}
		if config.Mode == "oauth" && config.OAuthUser == user {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Server) githubTeamSourceIDs(ctx context.Context, due bool) ([]string, error) {
	rows, err := s.DB.Query(ctx, "SELECT id FROM github_team_sync_sources WHERE enabled AND (NOT $1 OR (next_check_at<=now() AND refresh_until<=now())) ORDER BY next_check_at,id LIMIT 8", due)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Server) runGithubTeamSync(ctx context.Context) {
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		ids, err := s.githubTeamSourceIDs(ctx, true)
		if err != nil && ctx.Err() == nil {
			slog.Error("GitHub directory schedule unavailable")
		}
		for _, id := range ids {
			refresh, cancel := context.WithTimeout(ctx, time.Minute)
			_, err = s.reconcileGithubTeamSource(refresh, id, "GitHub directory scheduler", "", true, 0, "")
			cancel()
			if err != nil && ctx.Err() == nil && !errors.Is(err, errGithubDirectoryConflict) {
				slog.Warn("GitHub directory synchronization failed; membership retained")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
