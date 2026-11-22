package control

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

func totpCode(secret []byte, step int64) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 15
	number := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", number%1000000)
}

func matchTOTPStep(secret []byte, code string, last int64, now time.Time) (int64, bool) {
	if len(code) != 6 {
		return 0, false
	}
	step := now.Unix() / 30
	for _, candidate := range []int64{step, step - 1, step + 1} {
		if candidate > last && subtle.ConstantTimeCompare([]byte(totpCode(secret, candidate)), []byte(code)) == 1 {
			return candidate, true
		}
	}
	return 0, false
}

func (s *Server) verifySecondFactor(ctx context.Context, tx pgx.Tx, user, code string) (bool, error) {
	var encrypted []byte
	var last int64
	err := tx.QueryRow(ctx, "SELECT secret,last_step FROM account_totp WHERE user_id=$1 AND confirmed FOR UPDATE", user).Scan(&encrypted, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		encrypted = nil
	} else if err != nil {
		return false, err
	}
	if encrypted == nil {
		result, err := tx.Exec(ctx, "DELETE FROM recovery_codes WHERE user_id=$1 AND code_hash=$2", user, tokenHash(code))
		return result.RowsAffected() == 1, err
	}
	secret, err := unseal(s.Key, encrypted)
	if err != nil {
		return false, err
	}
	if step, ok := matchTOTPStep(secret, code, last, time.Now()); ok {
		_, err = tx.Exec(ctx, "UPDATE account_totp SET last_step=$2 WHERE user_id=$1", user, step)
		return err == nil, err
	}
	result, err := tx.Exec(ctx, "DELETE FROM recovery_codes WHERE user_id=$1 AND code_hash=$2", user, tokenHash(code))
	return result.RowsAffected() == 1, err
}

func (s *Server) secondFactorStatus(w http.ResponseWriter, r *http.Request) {
	var enrolled, required bool
	err := s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM account_totp WHERE user_id=$1 AND confirmed),
 c.require_second_factor OR COALESCE((SELECT require_second_factor FROM teams WHERE id=$2),false) FROM control_settings c WHERE id=1`, actor(r).ID, actor(r).Team).Scan(&enrolled, &required)
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	var passkey bool
	if s.DB.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM passkeys WHERE user_id=$1)", actor(r).ID).Scan(&passkey) != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	var unused int
	if s.DB.QueryRow(r.Context(), "SELECT count(*) FROM recovery_codes WHERE user_id=$1", actor(r).ID).Scan(&unused) != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"recovery_codes_remaining": unused, "totp_enrolled": enrolled, "passkey_enrolled": passkey, "second_factor_required": required, "enrolment_required": required && !enrolled && !passkey})
}

func (s *Server) beginTOTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"current_password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !s.limitAttempts(w, r, "totp-enrol:"+actor(r).ID, 5) {
		return
	}
	var hash []byte
	if err := s.DB.QueryRow(r.Context(), "SELECT password_hash FROM users WHERE id=$1", actor(r).ID).Scan(&hash); err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	if len(hash) > 0 && (len(in.Password) > 72 || bcrypt.CompareHashAndPassword(hash, []byte(in.Password)) != nil) {
		http.Error(w, "current password is incorrect", 400)
		return
	}
	secret := make([]byte, 20)
	if _, err := rand.Read(secret); err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	encrypted, err := seal(s.Key, secret)
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	result, err := s.DB.Exec(r.Context(), `INSERT INTO account_totp(user_id,secret) VALUES($1,$2) ON CONFLICT(user_id) DO UPDATE SET secret=$2,expires_at=now()+interval '10 minutes',last_step=-1 WHERE NOT account_totp.confirmed`, actor(r).ID, encrypted)
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	if result.RowsAffected() == 0 {
		http.Error(w, "second factor already enrolled", 409)
		return
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
	q := url.Values{"secret": {encoded}, "issuer": {"Cove"}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	respond(w, 201, map[string]string{"provisioning_uri": "otpauth://totp/" + url.PathEscape("Cove:"+actor(r).Email) + "?" + q.Encode()})
}

func (s *Server) confirmTOTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !s.limitAttempts(w, r, "totp-confirm:"+actor(r).ID, 5) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	cookieForCheck, cookieError := r.Cookie("__Host-cove")
	if cookieError != nil {
		http.Error(w, "sign in to enrol a second factor", 401)
		return
	}
	var live bool
	err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM sessions WHERE token_hash=$2 AND user_id=$1 AND expires_at>now()) FROM users WHERE id=$1 AND NOT disabled FOR SHARE", actor(r).ID, tokenHash(cookieForCheck.Value)).Scan(&live)
	if err != nil || !live {
		http.Error(w, "sign in to enrol a second factor", 401)
		return
	}
	var encrypted []byte
	err = tx.QueryRow(r.Context(), "SELECT secret FROM account_totp WHERE user_id=$1 AND NOT confirmed AND expires_at>now() FOR UPDATE", actor(r).ID).Scan(&encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "enrolment missing or expired", 400)
		return
	}
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	secret, err := unseal(s.Key, encrypted)
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	step, ok := matchTOTPStep(secret, in.Code, -1, time.Now())
	if !ok {
		http.Error(w, "invalid code", 400)
		return
	}
	_, err = tx.Exec(r.Context(), "UPDATE account_totp SET confirmed=true,last_step=$2 WHERE user_id=$1", actor(r).ID, step)
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM recovery_codes WHERE user_id=$1", actor(r).ID)
	}
	codes := make([]string, 10)
	for i := range codes {
		codes[i] = randomToken()
		if err == nil {
			_, err = tx.Exec(r.Context(), "INSERT INTO recovery_codes(user_id,code_hash) VALUES($1,$2)", actor(r).ID, tokenHash(codes[i]))
		}
	}
	cookie, cookieErr := r.Cookie("__Host-cove")
	if cookieErr != nil {
		http.Error(w, "sign in to enrol a second factor", 401)
		return
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "DELETE FROM sessions WHERE user_id=$1 AND token_hash<>$2", actor(r).ID, tokenHash(cookie.Value))
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "UPDATE sessions SET second_factor_verified=true WHERE user_id=$1 AND token_hash=$2", actor(r).ID, tokenHash(cookie.Value))
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

func (s *Server) secondFactorChallenge(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "origin rejected", 403)
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	cookie, err := r.Cookie("__Host-cove")
	if err != nil {
		http.Error(w, "unauthorized", 401)
		return
	}
	var user string
	err = s.DB.QueryRow(r.Context(), `SELECT s.user_id FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>now() AND NOT u.disabled AND u.email_verified AND NOT s.second_factor_verified AND s.expires_at>now()+interval '11 hours 50 minutes'`, tokenHash(cookie.Value)).Scan(&user)
	if err != nil {
		http.Error(w, "invalid or expired challenge", 401)
		return
	}
	if !s.authLimit(w, r) || !s.limitAttempts(w, r, "second-factor:"+user, 5) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	// Match the lock order used by password rotation before consuming a factor.
	var id string
	err = tx.QueryRow(r.Context(), "SELECT id FROM users WHERE id=$1 AND NOT disabled FOR SHARE", user).Scan(&id)
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	ok, err := s.verifySecondFactor(r.Context(), tx, user, in.Code)
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	if !ok {
		http.Error(w, "invalid code", 401)
		return
	}
	token := randomToken()
	expiry := time.Now().Add(12 * time.Hour)
	result, err := tx.Exec(r.Context(), "DELETE FROM sessions WHERE token_hash=$1 AND NOT second_factor_verified", tokenHash(cookie.Value))
	if err == nil && result.RowsAffected() != 1 {
		http.Error(w, "invalid or expired challenge", 401)
		return
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO sessions(token_hash,user_id,expires_at,user_agent,second_factor_verified) VALUES($1,$2,$3,$4,true)", tokenHash(token), user, expiry, sessionUserAgent(r))
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	setSession(w, token, expiry)
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) resetSecondFactor(w http.ResponseWriter, r *http.Request) {
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "authentication unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	user := r.PathValue("id")
	var id string
	err = tx.QueryRow(r.Context(), "SELECT id FROM users WHERE id=$1 FOR UPDATE", user).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	for _, query := range []string{"DELETE FROM account_totp WHERE user_id=$1", "DELETE FROM passkeys WHERE user_id=$1", "DELETE FROM webauthn_challenges WHERE user_id=$1", "DELETE FROM recovery_codes WHERE user_id=$1", "DELETE FROM sessions WHERE user_id=$1", "DELETE FROM oauth_states WHERE link_user_id=$1", "UPDATE users SET auth_version=auth_version+1 WHERE id=$1"} {
		if err == nil {
			_, err = tx.Exec(r.Context(), query, user)
		}
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not reset second factor", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *Server) secondFactorPolicy(w http.ResponseWriter, r *http.Request) {
	query := "SELECT require_second_factor FROM control_settings WHERE id=1"
	args := []any{}
	if r.PathValue("id") != "" {
		query = "SELECT require_second_factor FROM teams WHERE id=$1"
		args = append(args, r.PathValue("id"))
	}
	var in struct {
		Required bool `json:"require_second_factor"`
	}
	if r.Method == "GET" {
		if err := s.DB.QueryRow(r.Context(), query, args...).Scan(&in.Required); err != nil {
			http.Error(w, "policy unavailable", 503)
			return
		}
		respond(w, 200, in)
		return
	}
	if !decode(w, r, &in) {
		return
	}
	query = "UPDATE control_settings SET require_second_factor=$1 WHERE id=1"
	args = []any{in.Required}
	if r.PathValue("id") != "" {
		allowed := actor(r).Admin
		for _, m := range actor(r).Teams {
			if m.ID == r.PathValue("id") && m.Role == "owner" {
				allowed = true
			}
		}
		if !allowed {
			http.Error(w, "team owner required", 403)
			return
		}
		query = "UPDATE teams SET require_second_factor=$1 WHERE id=$2"
		args = append(args, r.PathValue("id"))
	}
	if _, err := s.DB.Exec(r.Context(), query, args...); err != nil {
		http.Error(w, "policy unavailable", 503)
		return
	}
	respond(w, 200, in)
}
