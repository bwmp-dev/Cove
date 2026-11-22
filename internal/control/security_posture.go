package control

import (
	"net/http"
	"net/url"
	"os"
	"strings"
)

type postureCheck struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
}

func (s *Server) securityPosture(w http.ResponseWriter, r *http.Request) {
	var secondFactor, passwordDisabled, registration, enrolled, stale, expiring, audit bool
	var allowlist []string
	err := s.DB.QueryRow(r.Context(), `SELECT require_second_factor,password_login_disabled,(registration_enabled OR oidc_registration_enabled),panel_allowlist,
 NOT EXISTS(SELECT 1 FROM users u WHERE u.is_admin AND NOT u.disabled AND NOT EXISTS(SELECT 1 FROM account_totp t WHERE t.user_id=u.id AND t.confirmed) AND NOT EXISTS(SELECT 1 FROM passkeys p WHERE p.user_id=u.id)),
 NOT EXISTS(SELECT 1 FROM nodes WHERE state<>'pending' AND (last_checked_at IS NULL OR last_checked_at<now()-interval '15 minutes')),
 NOT EXISTS(SELECT 1 FROM api_tokens WHERE expires_at IS NULL),
 EXISTS(SELECT 1 FROM audit_export_settings WHERE (webhook_enabled OR syslog_enabled) AND retention_days BETWEEN 1 AND 3650)
 FROM control_settings WHERE id=1`).Scan(&secondFactor, &passwordDisabled, &registration, &allowlist, &enrolled, &stale, &expiring, &audit)
	if err != nil {
		http.Error(w, "security posture unavailable", 503)
		return
	}
	checks := []postureCheck{}
	add := func(id string, pass bool, message, remediation string) {
		status := "warn"
		if pass {
			status = "pass"
		}
		checks = append(checks, postureCheck{id, status, message, remediation})
	}
	add("admin_second_factor_required", secondFactor, "Instance second-factor policy evaluated.", "Require second factor in Settings → Authentication.")
	add("admin_second_factor_enrolled", enrolled, "Enabled administrator TOTP and passkey enrollment evaluated.", "Enroll each administrator in TOTP or a passkey before enforcing the policy.")
	add("password_login", passwordDisabled, "Password login policy evaluated.", "Consider disabling password login after testing another sign-in method and the documented host recovery procedure.")
	add("registration", !registration, "Open email and OIDC registration policies evaluated.", "Disable open registration in Settings when accounts should be invitation-only.")
	if override, ok := os.LookupEnv("COVE_PANEL_ALLOWLIST"); ok {
		allowlist = nil
		if override != "" {
			allowlist = strings.Split(override, ",")
		}
	}
	add("panel_allowlist", len(allowlist) > 0, "Effective panel network allowlist evaluated.", "Configure Panel access and include the current client address; retain host access for recovery.")
	u, err := url.Parse(s.PublicURL)
	add("https", err == nil && u.Scheme == "https" && u.Host != "", "Configured public origin evaluated; certificate validity is deployment-specific.", "Set COVE_PUBLIC_URL to the externally reachable HTTPS origin and verify the certificate from a client.")
	add("host_checks", stale, "Enrolled nodes checked within the last 15 minutes.", "Inspect disconnected nodes and run Verify after resolving SSH or worker failures.")
	add("token_expiry", expiring, "Workload API token expiry evaluated; the host administrator token is a separate permanent recovery credential.", "Revoke tokens without expiry and replace them with short-lived scoped tokens; protect the host administrator token.")
	add("audit_export_retention", audit, "Audit export and bounded retention configuration evaluated.", "Enable an audit export destination and choose a retention period in Settings → Audit export.")
	if err := s.observedPosture(r.Context(), add); err != nil {
		http.Error(w, "security observations unavailable", 503)
		return
	}
	respond(w, 200, map[string]any{"checks": checks})
}
