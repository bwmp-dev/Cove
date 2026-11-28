package control

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
	"github.com/jackc/pgx/v5"
)

type ldapAccount struct {
	Subject, Email, Name string
	Groups               []*ldap.DN
}

var errLDAPCredentials = errors.New("invalid directory credentials")

func authenticateLDAP(ctx context.Context, c ldapConfiguration, username, password string) (ldapAccount, error) {
	var account ldapAccount
	if password == "" {
		return account, errLDAPCredentials
	}
	if c.BindDN == "" || c.Password == "" {
		return account, errors.New("directory service credentials required")
	}
	connection, err := dialLDAP(ctx, c)
	if err != nil {
		return account, err
	}
	defer connection.Close()
	if err = connection.Bind(c.BindDN, c.Password); err != nil {
		return account, errors.New("directory service bind failed")
	}
	filter := strings.ReplaceAll(c.Filter, "{username}", ldap.EscapeFilter(username))
	result, err := connection.Search(ldap.NewSearchRequest(c.Base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 8, false, filter, []string{c.Subject, c.Email, c.NameAttribute}, nil))
	if err != nil || len(result.Referrals) > 0 {
		return account, errors.New("directory user search failed")
	}
	if len(result.Entries) != 1 {
		return account, errLDAPCredentials
	}
	entry := result.Entries[0]
	base, _ := ldap.ParseDN(c.Base)
	dn, err := ldap.ParseDN(entry.DN)
	if err != nil || !base.AncestorOfFold(dn) {
		return account, errors.New("directory user is outside the search base")
	}
	identities := entry.GetRawAttributeValues(c.Subject)
	if len(identities) != 1 || len(identities[0]) == 0 || len(identities[0]) > 128 {
		return account, errors.New("immutable directory subject required")
	}
	account.Subject = hex.EncodeToString(identities[0])
	emails := entry.GetAttributeValues(c.Email)
	if len(emails) != 1 {
		return account, errors.New("one verified directory email required")
	}
	var valid bool
	account.Email, valid = normalizeEmail(emails[0])
	if !valid {
		return account, errors.New("valid directory email required")
	}
	account.Name = strings.TrimSpace(entry.GetAttributeValue(c.NameAttribute))
	if account.Name == "" || len(account.Name) > 120 {
		account.Name = "Directory user"
	}
	if err = connection.Bind(entry.DN, password); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
			return account, errLDAPCredentials
		}
		return account, errors.New("directory user bind failed")
	}
	if err = connection.Bind(c.BindDN, c.Password); err != nil {
		return account, errors.New("directory service bind failed")
	}
	groupFilter := strings.NewReplacer("{username}", ldap.EscapeFilter(username), "{dn}", ldap.EscapeFilter(entry.DN)).Replace(c.GroupFilter)
	groups, err := connection.Search(ldap.NewSearchRequest(c.GroupBase, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 1000, 8, false, groupFilter, []string{"1.1"}, nil))
	if err != nil || len(groups.Referrals) > 0 {
		return account, errors.New("complete directory group search required")
	}
	groupBase, _ := ldap.ParseDN(c.GroupBase)
	for _, entry := range groups.Entries {
		dn, e := ldap.ParseDN(entry.DN)
		if e != nil || !groupBase.AncestorOfFold(dn) {
			return account, errors.New("directory group is outside the search base")
		}
		account.Groups = append(account.Groups, dn)
	}
	return account, nil
}

func (s *Server) ldapLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email           string `json:"email"`
		Password        string `json:"password"`
		Link            bool   `json:"link"`
		CurrentPassword string `json:"current_password"`
		Code            string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	username := strings.ToLower(strings.TrimSpace(in.Email))
	if username == "" || len(username) > 254 || strings.ContainsAny(username, "\x00\r\n") || in.Password == "" || len(in.Password) > 4096 {
		http.Error(w, "invalid directory sign-in request", 400)
		return
	}
	id := r.PathValue("provider")
	p, err := s.loadLDAP(r.Context(), s.DB, id)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !p.Enabled {
		http.Error(w, "directory provider unavailable", 404)
		return
	}
	if err != nil {
		http.Error(w, "directory provider unavailable", 503)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	account, err := authenticateLDAP(ctx, p.Config, username, in.Password)
	if errors.Is(err, errLDAPCredentials) {
		http.Error(w, "invalid directory credentials", 401)
		return
	}
	if err != nil {
		http.Error(w, "directory authentication unavailable; no membership changes applied", 502)
		return
	}
	tx, err := s.identityTransaction(r.Context())
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	current, err := s.loadLDAP(r.Context(), tx, id)
	if err != nil || !current.Enabled || current.Version != p.Version {
		http.Error(w, "directory settings changed; sign in again", 409)
		return
	}
	var user string
	var disabled bool
	err = tx.QueryRow(r.Context(), "SELECT u.id,u.disabled FROM ldap_identities i JOIN users u ON u.id=i.user_id WHERE i.provider_id=$1 AND i.subject=$2 FOR UPDATE OF u", id, account.Subject).Scan(&user, &disabled)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	if disabled {
		http.Error(w, "account disabled", 403)
		return
	}
	if in.Link {
		local, e := s.sessionIdentity(r)
		if e != nil {
			http.Error(w, "sign in before linking a directory identity", 401)
			return
		}
		if user != "" && user != local.ID || local.Email != account.Email {
			http.Error(w, "directory identity is already assigned or its email differs from this account", 409)
			return
		}
		proof, _ := json.Marshal(map[string]string{"current_password": in.CurrentPassword, "code": in.Code})
		request := r.Clone(context.WithValue(r.Context(), identityKey{}, local))
		request.Body = io.NopCloser(bytes.NewReader(proof))
		if !s.reauthenticate(w, request, tx) {
			return
		}
		user = local.ID
	} else if user == "" {
		var exists bool
		if tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM users WHERE email=$1)", account.Email).Scan(&exists) != nil {
			http.Error(w, "authentication unavailable", 503)
			return
		}
		if exists {
			http.Error(w, "account exists; sign in and explicitly link this directory identity", 409)
			return
		}
		if !p.Config.Registration {
			http.Error(w, "directory registration is disabled; ask an administrator for an invitation and link the identity", 403)
			return
		}
		user = resourceID()
		_, err = tx.Exec(r.Context(), "INSERT INTO users(id,email,name,email_verified) VALUES($1,$2,$3,true)", user, account.Email, account.Name)
	}
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		_, err = tx.Exec(r.Context(), `INSERT INTO ldap_identities(provider_id,subject,user_id,username) VALUES($1,$2,$3,$4) ON CONFLICT(provider_id,subject) DO UPDATE SET username=EXCLUDED.username WHERE ldap_identities.user_id=EXCLUDED.user_id`, id, account.Subject, user, username)
	}
	if err != nil {
		http.Error(w, "directory identity is already linked or unavailable", 409)
		return
	}
	mappings, err := loadDirectoryMappings(r.Context(), tx, "ldap", id)
	claims := []directoryClaim{}
	if err == nil {
		for _, mapping := range mappings {
			target, e := ldap.ParseDN(mapping.Group)
			if e != nil {
				err = e
				break
			}
			for _, group := range account.Groups {
				if group.EqualFold(target) {
					role, e := directoryRole(r.Context(), tx, mapping.Team, mapping.Role)
					if e != nil {
						err = e
						break
					}
					claims = append(claims, directoryClaim{Group: mapping.Group, Team: mapping.Team, User: user, memberInput: role})
					break
				}
			}
		}
	}
	if err == nil {
		_, err = syncDirectoryMemberships(r.Context(), tx, "ldap", id, user, claims, user)
	}
	var required bool
	if err == nil {
		err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM account_totp WHERE user_id=$1 AND confirmed) OR EXISTS(SELECT 1 FROM passkeys WHERE user_id=$1)", user).Scan(&required)
	}
	token := randomToken()
	expiry := time.Now().Add(12 * time.Hour)
	if err == nil && !in.Link {
		_, err = tx.Exec(r.Context(), "INSERT INTO sessions(token_hash,user_id,expires_at,user_agent) VALUES($1,$2,$3,$4)", tokenHash(token), user, expiry, sessionUserAgent(r))
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO audit_events(actor,action,subject,status) VALUES($1,$2,$3,200)", user, "LDAP authentication", id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "authentication unavailable; no membership changes applied", 503)
		return
	}
	if !in.Link {
		s.completeSignIn(w, r, user, token, expiry)
	}
	respond(w, 200, map[string]any{"user": user, "linked": in.Link, "second_factor_required": required})
}
