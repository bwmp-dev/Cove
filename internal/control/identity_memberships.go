package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

type directoryMapping struct {
	Group string `json:"group"`
	Team  string `json:"team_id"`
	Role  string `json:"role"`
}

type directoryClaim struct {
	Group string
	Team  string
	User  string
	memberInput
}

type directoryMemberChange struct {
	Team    string       `json:"team_id"`
	User    string       `json:"user_id"`
	Name    string       `json:"user_name,omitempty"`
	Email   string       `json:"email,omitempty"`
	Before  *memberInput `json:"before"`
	After   *memberInput `json:"after"`
	Warning string       `json:"warning,omitempty"`
}

func (s *Server) identityTransaction(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	// Account disablement and directory grants share the administrator recovery lock.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1129272902)"); err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

func directoryRole(ctx context.Context, tx pgx.Tx, team, role string) (memberInput, error) {
	out := memberInput{Role: "member", Grants: []grant{}}
	switch role {
	case "owner", "admin", "member":
		out.Role = role
	case "viewer", "operator", "editor":
		out.Grants = []grant{{Scope: "team", ScopeID: team, Role: role}}
	default:
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM team_roles WHERE team_id=$1 AND id=$2)", team, role).Scan(&exists); err != nil {
			return out, err
		}
		if !exists {
			return out, errors.New("unknown team role")
		}
		out.Grants = []grant{{Scope: "team", ScopeID: team, Role: role}}
	}
	return out, nil
}

func saveDirectoryMappings(ctx context.Context, tx pgx.Tx, kind, source string, mappings []directoryMapping) error {
	if len(mappings) > 100 {
		return errors.New("at most 100 group mappings are allowed")
	}
	seen := map[string]bool{}
	ordered := append([]directoryMapping{}, mappings...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Team+"\x00"+ordered[i].Group < ordered[j].Team+"\x00"+ordered[j].Group
	})
	for _, mapping := range ordered {
		key := mapping.Group + "\x00" + mapping.Team
		if mapping.Group == "" || len(mapping.Group) > 2048 || strings.ContainsAny(mapping.Group, "\x00\r\n") || seen[key] {
			return errors.New("unique group and team mappings are required")
		}
		seen[key] = true
		var found string
		if err := tx.QueryRow(ctx, "SELECT id FROM teams WHERE id=$1 FOR UPDATE", mapping.Team).Scan(&found); errors.Is(err, pgx.ErrNoRows) {
			return errors.New("unknown Cove team")
		} else if err != nil {
			return err
		}
		if _, err := directoryRole(ctx, tx, mapping.Team, mapping.Role); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM identity_group_mappings WHERE source_kind=$1 AND source_id=$2", kind, source); err != nil {
		return err
	}
	for _, mapping := range mappings {
		if _, err := tx.Exec(ctx, "INSERT INTO identity_group_mappings(source_kind,source_id,group_id,team_id,role) VALUES($1,$2,$3,$4,$5)", kind, source, mapping.Group, mapping.Team, mapping.Role); err != nil {
			return err
		}
	}
	return nil
}

func loadDirectoryMappings(ctx context.Context, tx pgx.Tx, kind, source string) ([]directoryMapping, error) {
	rows, err := tx.Query(ctx, "SELECT group_id,team_id,role FROM identity_group_mappings WHERE source_kind=$1 AND source_id=$2 ORDER BY group_id,team_id", kind, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []directoryMapping{}
	for rows.Next() {
		var mapping directoryMapping
		if err = rows.Scan(&mapping.Group, &mapping.Team, &mapping.Role); err != nil {
			return nil, err
		}
		out = append(out, mapping)
	}
	return out, rows.Err()
}

func canonicalDirectoryGrants(in []grant) []grant {
	out := append([]grant{}, in...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Scope+"\x00"+a.ScopeID+"\x00"+a.Role < b.Scope+"\x00"+b.ScopeID+"\x00"+b.Role
	})
	return slices.Compact(out)
}

func mergeDirectoryRole(current *memberInput, next memberInput) *memberInput {
	if current == nil {
		current = &memberInput{Role: next.Role, Grants: []grant{}}
	}
	priority := map[string]int{"member": 0, "admin": 1, "owner": 2}
	if priority[next.Role] > priority[current.Role] {
		current.Role = next.Role
	}
	current.Grants = canonicalDirectoryGrants(append(current.Grants, next.Grants...))
	if current.Role != "member" {
		current.Grants = []grant{}
	}
	return current
}

// The caller holds identityTransaction's account lock; team locks also serialize manual owner changes.
func syncDirectoryMemberships(ctx context.Context, tx pgx.Tx, kind, source, user string, desired []directoryClaim, by string) ([]directoryMemberChange, error) {
	type pair struct{ team, user string }
	affected := map[pair]bool{}
	rows, err := tx.Query(ctx, "SELECT team_id,user_id FROM identity_membership_claims WHERE source_kind=$1 AND source_id=$2 AND ($3='' OR user_id=$3)", kind, source, user)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key pair
		if err = rows.Scan(&key.team, &key.user); err != nil {
			rows.Close()
			return nil, err
		}
		affected[key] = true
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, claim := range desired {
		if user != "" && claim.User != user {
			return nil, errors.New("directory reconciliation user differs")
		}
		affected[pair{claim.Team, claim.User}] = true
	}
	keys := make([]pair, 0, len(affected))
	for key := range affected {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].team+"\x00"+keys[i].user < keys[j].team+"\x00"+keys[j].user })
	locked := ""
	for _, key := range keys {
		if key.team != locked {
			if err = tx.QueryRow(ctx, "SELECT id FROM teams WHERE id=$1 FOR UPDATE", key.team).Scan(&locked); err != nil {
				return nil, err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO identity_membership_bases(team_id,user_id,role,grants)
 SELECT $1,$2,m.role,COALESCE(m.grants,'[]'::jsonb) FROM (SELECT 1) v LEFT JOIN team_members m ON m.team_id=$1 AND m.user_id=$2 ON CONFLICT DO NOTHING`, key.team, key.user)
		if err != nil {
			return nil, err
		}
	}
	if _, err = tx.Exec(ctx, "DELETE FROM identity_membership_claims WHERE source_kind=$1 AND source_id=$2 AND ($3='' OR user_id=$3)", kind, source, user); err != nil {
		return nil, err
	}
	for _, claim := range desired {
		if _, err = tx.Exec(ctx, "INSERT INTO identity_membership_claims(source_kind,source_id,group_id,team_id,user_id,role,grants) VALUES($1,$2,$3,$4,$5,$6,$7)", kind, source, claim.Group, claim.Team, claim.User, claim.Role, claim.Grants); err != nil {
			return nil, err
		}
	}
	changes := []directoryMemberChange{}
	for _, key := range keys {
		change, e := resolveDirectoryMember(ctx, tx, key.team, key.user)
		if e != nil {
			return nil, e
		}
		if change == nil {
			continue
		}
		changes = append(changes, *change)
		details, e := json.Marshal(map[string]any{"source_kind": kind, "source_id": source, "change": change})
		if e != nil {
			return nil, e
		}
		if _, err = tx.Exec(ctx, "INSERT INTO audit_events(actor,action,subject,status,team_id,details) VALUES($1,'identity membership reconciled',$2,200,$3,$4)", by, key.user, key.team, details); err != nil {
			return nil, err
		}
	}
	return changes, nil
}

func resolveDirectoryMember(ctx context.Context, tx pgx.Tx, team, user string) (*directoryMemberChange, error) {
	var before memberInput
	err := tx.QueryRow(ctx, "SELECT role,grants FROM team_members WHERE team_id=$1 AND user_id=$2", team, user).Scan(&before.Role, &before.Grants)
	change := &directoryMemberChange{Team: team, User: user}
	if err == nil {
		before.Grants = canonicalDirectoryGrants(before.Grants)
		change.Before = &before
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var baseRole *string
	var baseGrants []grant
	if err = tx.QueryRow(ctx, "SELECT role,grants FROM identity_membership_bases WHERE team_id=$1 AND user_id=$2", team, user).Scan(&baseRole, &baseGrants); err != nil {
		return nil, err
	}
	if baseRole != nil {
		change.After = mergeDirectoryRole(nil, memberInput{Role: *baseRole, Grants: baseGrants})
	}
	rows, err := tx.Query(ctx, "SELECT role,grants FROM identity_membership_claims WHERE team_id=$1 AND user_id=$2", team, user)
	if err != nil {
		return nil, err
	}
	claims := 0
	for rows.Next() {
		var role memberInput
		if err = rows.Scan(&role.Role, &role.Grants); err != nil {
			rows.Close()
			return nil, err
		}
		change.After = mergeDirectoryRole(change.After, role)
		claims++
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if change.Before != nil && before.Role == "owner" && (change.After == nil || change.After.Role != "owner") {
		var another bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM team_members m JOIN users u ON u.id=m.user_id WHERE m.team_id=$1 AND m.user_id<>$2 AND m.role='owner' AND NOT u.disabled)", team, user).Scan(&another); err != nil {
			return nil, err
		}
		if !another {
			change.After = &memberInput{Role: "owner", Grants: []grant{}}
			change.Warning = "The last active team owner was retained as a local owner."
			if _, err = tx.Exec(ctx, "UPDATE identity_membership_bases SET role='owner',grants='[]' WHERE team_id=$1 AND user_id=$2", team, user); err != nil {
				return nil, err
			}
		}
	}
	if change.After == nil {
		_, err = tx.Exec(ctx, "DELETE FROM team_members WHERE team_id=$1 AND user_id=$2", team, user)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO team_members(team_id,user_id,role,grants) VALUES($1,$2,$3,$4)
 ON CONFLICT(team_id,user_id) DO UPDATE SET role=EXCLUDED.role,grants=EXCLUDED.grants`, team, user, change.After.Role, change.After.Grants)
	}
	if err != nil {
		return nil, err
	}
	if claims == 0 {
		if _, err = tx.Exec(ctx, "DELETE FROM identity_membership_bases WHERE team_id=$1 AND user_id=$2", team, user); err != nil {
			return nil, err
		}
	}
	if change.Warning == "" && ((change.Before == nil && change.After == nil) || (change.Before != nil && change.After != nil && change.Before.Role == change.After.Role && slices.Equal(change.Before.Grants, change.After.Grants))) {
		return nil, nil
	}
	return change, nil
}

func directoryMappingClaims(ctx context.Context, tx pgx.Tx, mappings []directoryMapping, members map[string][]string) ([]directoryClaim, error) {
	out := []directoryClaim{}
	for _, mapping := range mappings {
		role, err := directoryRole(ctx, tx, mapping.Team, mapping.Role)
		if err != nil {
			return nil, fmt.Errorf("mapping role unavailable: %w", err)
		}
		for _, user := range slices.Compact(slices.Sorted(slices.Values(members[mapping.Group]))) {
			out = append(out, directoryClaim{Group: mapping.Group, Team: mapping.Team, User: user, memberInput: role})
		}
	}
	return out, nil
}
