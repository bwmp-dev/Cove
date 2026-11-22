package control

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// Lock the account before checking credentials so concurrent security changes
// cannot invalidate the proof between verification and the mutation.
func (s *Server) reauthenticate(w http.ResponseWriter, r *http.Request, tx pgx.Tx) bool {
	var in struct {
		Password string `json:"current_password"`
		Code     string `json:"code"`
	}
	if !decode(w, r, &in) || !s.limitAttempts(w, r, "security-reauth:"+actor(r).ID, 5) {
		return false
	}
	var hash []byte
	if err := tx.QueryRow(r.Context(), "SELECT password_hash FROM users WHERE id=$1 AND NOT disabled AND email_verified FOR UPDATE", actor(r).ID).Scan(&hash); err != nil {
		http.Error(w, "authentication unavailable", 503)
		return false
	}
	cookie, err := r.Cookie("__Host-cove")
	var live, recentFactor bool
	if err != nil {
		http.Error(w, "sign in again", 401)
		return false
	}
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM sessions WHERE token_hash=$1 AND user_id=$2 AND expires_at>now()), EXISTS(SELECT 1 FROM sessions WHERE token_hash=$1 AND user_id=$2 AND second_factor_verified AND expires_at>now()+interval '11 hours 50 minutes')", tokenHash(cookie.Value), actor(r).ID).Scan(&live, &recentFactor); err != nil {
		http.Error(w, "authentication unavailable", 503)
		return false
	}
	if !live {
		http.Error(w, "sign in again", 401)
		return false
	}
	if in.Password == "" && in.Code == "" && recentFactor {
		return true
	}
	if len(hash) > 0 && len(in.Password) <= 72 && bcrypt.CompareHashAndPassword(hash, []byte(in.Password)) == nil {
		return true
	}
	ok, err := s.verifySecondFactor(r.Context(), tx, actor(r).ID, in.Code)
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return false
	}
	if !ok {
		http.Error(w, "reauthentication required", 403)
		return false
	}
	return true
}

func (s *Server) regenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if !s.reauthenticate(w, r, tx) {
		return
	}
	var enrolled bool
	err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM account_totp WHERE user_id=$1 AND confirmed) OR EXISTS(SELECT 1 FROM passkeys WHERE user_id=$1)", actor(r).ID).Scan(&enrolled)
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	if !enrolled {
		http.Error(w, "second factor not enrolled", 409)
		return
	}
	_, err = tx.Exec(r.Context(), "DELETE FROM recovery_codes WHERE user_id=$1", actor(r).ID)
	codes := make([]string, 10)
	for i := range codes {
		codes[i] = randomToken()
		if err == nil {
			_, err = tx.Exec(r.Context(), "INSERT INTO recovery_codes(user_id,code_hash) VALUES($1,$2)", actor(r).ID, tokenHash(codes[i]))
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"recovery_codes": codes})
}

func (s *Server) removeTOTP(w http.ResponseWriter, r *http.Request) {
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if !s.reauthenticate(w, r, tx) {
		return
	}
	var required, passkey bool
	// Check every membership, rather than only the currently selected team.
	err = tx.QueryRow(r.Context(), `SELECT c.require_second_factor OR EXISTS(SELECT 1 FROM teams t WHERE t.require_second_factor AND (t.id=$2 OR EXISTS(SELECT 1 FROM team_members m WHERE m.team_id=t.id AND m.user_id=$1))), EXISTS(SELECT 1 FROM passkeys WHERE user_id=$1) FROM control_settings c WHERE id=1 FOR SHARE OF c`, actor(r).ID, actor(r).Team).Scan(&required, &passkey)
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	if required && !passkey {
		http.Error(w, "second factor required by policy", 409)
		return
	}
	result, err := tx.Exec(r.Context(), "DELETE FROM account_totp WHERE user_id=$1", actor(r).ID)
	if err == nil && result.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	if err == nil && !passkey {
		_, err = tx.Exec(r.Context(), "DELETE FROM recovery_codes WHERE user_id=$1", actor(r).ID)
	}
	if err == nil {
		err = s.invalidateSecurityFlows(r, tx)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) invalidateSecurityFlows(r *http.Request, tx pgx.Tx) error {
	cookie, err := r.Cookie("__Host-cove")
	if err != nil {
		return err
	}
	for _, query := range []string{
		"UPDATE users SET auth_version=auth_version+1 WHERE id=$1",
		"DELETE FROM sessions WHERE user_id=$1 AND token_hash<>$2",
		"DELETE FROM webauthn_challenges WHERE user_id=$1",
		"DELETE FROM oidc_states WHERE link_user_id=$1",
		"DELETE FROM oauth_states WHERE link_user_id=$1",
	} {
		args := []any{actor(r).ID}
		if query == "DELETE FROM sessions WHERE user_id=$1 AND token_hash<>$2" {
			args = append(args, tokenHash(cookie.Value))
		}
		if _, err = tx.Exec(r.Context(), query, args...); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) accountIdentities(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), `SELECT provider_id,subject FROM oidc_identities WHERE user_id=$1 UNION ALL SELECT 'github',github_id FROM users WHERE id=$1 AND github_id IS NOT NULL ORDER BY 1,2`, actor(r).ID)
	if err != nil {
		http.Error(w, "identities unavailable", 503)
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var provider, subject string
		if rows.Scan(&provider, &subject) != nil {
			http.Error(w, "identities unavailable", 503)
			return
		}
		out = append(out, map[string]string{"provider_id": provider, "subject": subject})
	}
	if rows.Err() != nil {
		http.Error(w, "identities unavailable", 503)
		return
	}
	respond(w, 200, out)
}

func (s *Server) unlinkIdentity(w http.ResponseWriter, r *http.Request) {
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "identities unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if !s.reauthenticate(w, r, tx) {
		return
	}
	provider := r.PathValue("provider")
	var subject string
	if provider == "github" {
		err = tx.QueryRow(r.Context(), "UPDATE users SET github_id=NULL WHERE id=$1 AND github_id IS NOT NULL RETURNING id", actor(r).ID).Scan(&subject)
	} else {
		err = tx.QueryRow(r.Context(), "DELETE FROM oidc_identities WHERE user_id=$1 AND provider_id=$2 RETURNING subject", actor(r).ID, provider).Scan(&subject)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	var usable bool
	if err == nil {
		config, _, e := s.integrationConfig(r.Context(), tx)
		if e != nil {
			http.Error(w, "identities unavailable", 503)
			return
		}
		githubEnabled := config.Github.ClientID != "" && config.Github.ClientSecret != ""
		err = tx.QueryRow(r.Context(), `SELECT (u.password_hash IS NOT NULL AND NOT c.password_login_disabled) OR (u.github_id IS NOT NULL AND $2) OR EXISTS(SELECT 1 FROM oidc_identities WHERE user_id=u.id) OR EXISTS(SELECT 1 FROM passkeys WHERE user_id=u.id) FROM users u CROSS JOIN control_settings c WHERE u.id=$1 AND c.id=1 FOR SHARE OF c`, actor(r).ID, githubEnabled).Scan(&usable)
	}
	if err == nil && !usable {
		http.Error(w, "last usable sign-in method", 409)
		return
	}
	if err == nil {
		err = s.invalidateSecurityFlows(r, tx)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "identities unavailable", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
