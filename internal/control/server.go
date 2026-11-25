package control

import (
	"context"
	"cove/internal/panelurl"
	"cove/internal/worker"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

//go:embed schema.sql install.sh.tmpl worker-update*.sh.tmpl
var assets embed.FS

type Server struct {
	requestGuardSlots     chan struct{}
	outboundState         *outboundProxyState
	ProviderTransport     http.RoundTripper
	CloudflareTransport   http.RoundTripper
	NotificationTransport http.RoundTripper
	DB                    *pgxpool.Pool
	Key                   []byte
	AdminToken            string
	PublicURL             string
	BinaryDir             string
	FrontendDir           string
	LocalSocket           string
	GithubClientID        string
	GithubClientSecret    string
	OAuthClient           *http.Client
	SMTP                  SMTPConfig
	SendMail              func(context.Context, string, string, string) error
	dummyPasswordHash     []byte
	// workerRPC replaces node transports in tests that simulate several workers.
	workerRPC     func(context.Context, string, worker.RPCRequest) (worker.RPCResponse, error)
	templateFetch func(context.Context, string, string, string) ([]byte, string, error)
	secretCommand secretCommandFunc
	workerStream  func(context.Context, string, worker.RPCRequest, io.Reader, func(worker.StreamResponse, io.Reader) error) error
}

func (s *Server) Init(ctx context.Context) error {
	s.outboundState = &outboundProxyState{}
	_, err := panelurl.Parse(s.PublicURL)
	if err != nil {
		return err
	}
	if len(s.Key) != 32 || len(s.AdminToken) < 32 {
		return errors.New("a 32-byte encryption key and at least 32-character admin token are required")
	}
	sql, err := assets.ReadFile("schema.sql")
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, string(sql))
	if err != nil {
		return err
	}
	check, err := seal(s.Key, []byte("cove encryption key v1"))
	if err != nil {
		return err
	}
	if _, err = s.DB.Exec(ctx, "INSERT INTO control_settings(id,encryption_check) VALUES(1,$1) ON CONFLICT DO NOTHING", check); err != nil {
		return err
	}
	if _, err = s.DB.Exec(ctx, "UPDATE control_settings SET setup_completed=true WHERE id=1 AND NOT setup_completed AND EXISTS(SELECT 1 FROM users WHERE id<>'bootstrap' OR password_hash IS NOT NULL)"); err != nil {
		return err
	}
	if err = s.DB.QueryRow(ctx, "SELECT encryption_check FROM control_settings WHERE id=1").Scan(&check); err != nil {
		return err
	}
	plain, err := unseal(s.Key, check)
	if err != nil || string(plain) != "cove encryption key v1" {
		return errors.New("COVE_ENCRYPTION_KEY does not match this database")
	}
	if err = s.initializeQuotaUsage(ctx); err != nil {
		return err
	}
	if err = s.importIntegrationSettings(ctx); err != nil {
		return err
	}
	if _, err = s.outboundConfig(ctx); err != nil {
		return err
	}
	s.dummyPasswordHash, err = bcrypt.GenerateFromPassword([]byte(randomToken()), 12)
	return err
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerPublicAPI(mux)
	mux.Handle("GET /api/search", s.admin(http.HandlerFunc(s.workspaceSearch)))
	for _, pattern := range []string{"GET /api/projects/{id}/status-share", "POST /api/projects/{id}/status-share", "DELETE /api/projects/{id}/status-share"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.projectStatusShare)))
	}
	mux.HandleFunc("GET /api/public/projects/{share}", s.publicProjectStatus)
	mux.HandleFunc("GET /status/{share}/badge.svg", s.publicProjectStatus)
	for _, pattern := range []string{"GET /api/projects/{id}/notes", "PUT /api/projects/{id}/notes", "GET /api/workloads/{id}/notes", "PUT /api/workloads/{id}/notes"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.workspaceNotes)))
	}
	for _, pattern := range []string{"GET /api/preferences/timezone", "PUT /api/preferences/timezone", "GET /api/teams/{id}/timezone", "PUT /api/teams/{id}/timezone"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.displayTimezone)))
	}
	mux.Handle("GET /api/activity", s.admin(http.HandlerFunc(s.workspaceActivity)))
	mux.Handle("GET /api/onboarding", s.admin(http.HandlerFunc(s.onboarding)))
	mux.Handle("PUT /api/onboarding", s.admin(http.HandlerFunc(s.onboarding)))
	mux.Handle("GET /api/resource-browser", s.admin(http.HandlerFunc(s.browseResources)))
	mux.Handle("POST /api/bulk-actions", s.admin(http.HandlerFunc(s.bulkResourceAction)))
	mux.Handle("GET /api/pins", s.admin(http.HandlerFunc(s.personalPins)))
	mux.Handle("PUT /api/pins", s.admin(http.HandlerFunc(s.personalPins)))
	mux.Handle("GET /api/workloads/{id}/tags", s.admin(http.HandlerFunc(s.resourceTags)))
	mux.Handle("PUT /api/workloads/{id}/tags", s.admin(http.HandlerFunc(s.resourceTags)))
	mux.Handle("GET /api/mcp", s.admin(http.HandlerFunc(s.mcpSettings)))
	mux.Handle("PUT /api/mcp", s.admin(http.HandlerFunc(s.mcpSettings)))
	for _, method := range []string{"GET", "POST", "DELETE"} {
		mux.HandleFunc(method+" /mcp", s.mcpHTTP)
	}
	for _, pattern := range []string{"GET /api/access-tokens", "POST /api/access-tokens", "GET /api/access-tokens/scopes", "DELETE /api/access-tokens/{token}"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.accessTokens)))
	}
	mux.Handle("POST /api/workloads/{id}/exec", s.admin(http.HandlerFunc(s.workloadExec)))
	mux.Handle("GET /api/database-engines", s.admin(http.HandlerFunc(s.databaseEngineOptions)))
	mux.Handle("GET /api/workloads/{id}/object-buckets", s.admin(http.HandlerFunc(s.objectBuckets)))
	mux.Handle("POST /api/workloads/{id}/object-buckets", s.admin(http.HandlerFunc(s.objectBuckets)))
	mux.Handle("POST /api/workloads/{id}/object-buckets/{bucket}/retry", s.admin(http.HandlerFunc(s.objectBuckets)))
	mux.Handle("DELETE /api/workloads/{id}/object-buckets/{bucket}", s.admin(http.HandlerFunc(s.objectBuckets)))
	mux.Handle("GET /api/workloads/{id}/database", s.admin(http.HandlerFunc(s.databaseDetails)))
	mux.Handle("POST /api/workloads/{id}/database/pooler", s.admin(http.HandlerFunc(s.databaseSettings)))
	mux.Handle("POST /api/workloads/{id}/database/public-access", s.admin(http.HandlerFunc(s.databaseSettings)))
	mux.Handle("POST /api/workloads/{id}/database/preflight", s.admin(http.HandlerFunc(s.databasePreflight)))
	mux.Handle("GET /api/workloads/{id}/database/credentials", s.admin(http.HandlerFunc(s.databaseDetails)))
	mux.Handle("GET /api/workloads/{id}/database/maintenance", s.admin(http.HandlerFunc(s.databaseMaintenance)))
	mux.Handle("POST /api/workloads/{id}/database/maintenance", s.admin(http.HandlerFunc(s.databaseMaintenance)))
	mux.Handle("POST /api/workloads/{id}/database/maintenance/{task}/discard", s.admin(http.HandlerFunc(s.discardDatabaseCopy)))
	mux.Handle("POST /api/workloads/{id}/database/maintenance/{task}/cutover", s.admin(http.HandlerFunc(s.cutoverDatabaseCopy)))
	mux.Handle("POST /api/workloads/{id}/database/maintenance/{task}/retry", s.admin(http.HandlerFunc(s.databaseMaintenance)))
	mux.Handle("GET /api/templates", s.admin(http.HandlerFunc(s.templates)))
	mux.Handle("POST /api/templates/draft", s.admin(http.HandlerFunc(s.templates)))
	mux.Handle("POST /api/workloads/{id}/template", s.admin(http.HandlerFunc(s.saveWorkloadTemplate)))
	mux.Handle("DELETE /api/teams/{id}/templates/{template}", s.admin(http.HandlerFunc(s.deleteTeamTemplate)))
	for _, prefix := range []string{"/api/settings/template-repositories", "/api/teams/{id}/template-repositories"} {
		mux.Handle("GET "+prefix, s.admin(http.HandlerFunc(s.templateRepositories)))
		mux.Handle("POST "+prefix, s.admin(http.HandlerFunc(s.templateRepositories)))
		mux.Handle("DELETE "+prefix+"/{repository}", s.admin(http.HandlerFunc(s.templateRepositories)))
		mux.Handle("POST "+prefix+"/{repository}/refresh", s.admin(http.HandlerFunc(s.templateRepositories)))
	}
	mux.Handle("GET /api/workloads/{id}/release-diff", s.admin(http.HandlerFunc(s.releaseDiff)))
	for _, method := range []string{"GET", "PUT"} {
		mux.Handle(method+" /api/teams/{id}/deployment-policy", s.admin(http.HandlerFunc(s.deploymentPolicy)))
	}
	mux.Handle("POST /api/operations/{id}/emergency-override", s.admin(http.HandlerFunc(s.emergencyOverride)))
	mux.Handle("POST /api/settings/update-status", s.admin(http.HandlerFunc(s.reportUpdateStatus)))
	mux.Handle("GET /api/settings/outbound-proxy/environment", s.admin(http.HandlerFunc(s.hostProxyEnvironment)))
	for _, method := range []string{"GET", "PUT"} {
		mux.Handle(method+" /api/settings/outbound-proxy", s.admin(http.HandlerFunc(s.outboundProxy)))
	}
	mux.Handle("GET /api/settings/security-posture", s.admin(http.HandlerFunc(s.securityPosture)))
	mux.Handle("POST /api/workloads/{id}/port-bindings", s.admin(http.HandlerFunc(s.portBindings)))
	mux.Handle("GET /api/settings/panel-access", s.admin(http.HandlerFunc(s.panelAccess)))
	mux.Handle("PUT /api/settings/panel-access", s.admin(http.HandlerFunc(s.panelAccess)))
	for _, method := range []string{"GET", "POST"} {
		mux.Handle(method+" /api/teams/{id}/roles", s.admin(http.HandlerFunc(s.customRoles)))
	}
	for _, method := range []string{"PUT", "DELETE"} {
		mux.Handle(method+" /api/teams/{id}/roles/{role}", s.admin(http.HandlerFunc(s.customRoles)))
	}
	mux.Handle("GET /api/settings/audit-export", s.admin(http.HandlerFunc(s.auditExportSettings)))
	mux.Handle("PUT /api/settings/audit-export", s.admin(http.HandlerFunc(s.auditExportSettings)))
	mux.HandleFunc("GET /auth/sign-in-methods", s.publicSignInMethods)
	mux.Handle("GET /api/account/login-history", s.admin(http.HandlerFunc(s.loginHistory)))
	mux.Handle("GET /api/users/{id}/login-history", s.admin(http.HandlerFunc(s.loginHistory)))
	mux.Handle("GET /api/settings/oidc/providers", s.admin(http.HandlerFunc(s.oidcProviders)))
	mux.Handle("GET /api/settings/github-team-sync", s.admin(http.HandlerFunc(s.githubTeamSources)))
	mux.Handle("GET /api/settings/directory/teams", s.admin(http.HandlerFunc(s.directoryTeams)))
	mux.Handle("GET /api/settings/scim/activity", s.admin(http.HandlerFunc(s.scimActivity)))
	mux.Handle("POST /api/settings/github-team-sync", s.admin(http.HandlerFunc(s.githubTeamSources)))
	mux.Handle("PUT /api/settings/github-team-sync/{source}", s.admin(http.HandlerFunc(s.githubTeamSources)))
	mux.Handle("DELETE /api/settings/github-team-sync/{source}", s.admin(http.HandlerFunc(s.githubTeamSources)))
	mux.Handle("POST /api/settings/github-team-sync/{source}/preview", s.admin(http.HandlerFunc(s.githubTeamPreview)))
	mux.Handle("POST /api/settings/github-team-sync/{source}/reconcile", s.admin(http.HandlerFunc(s.githubTeamPreview)))
	mux.Handle("GET /api/settings/scim", s.admin(http.HandlerFunc(s.scimSettings)))
	mux.Handle("PUT /api/settings/scim", s.admin(http.HandlerFunc(s.scimSettings)))
	mux.Handle("POST /api/settings/scim/token", s.admin(http.HandlerFunc(s.scimSettings)))
	mux.Handle("GET /api/settings/scim/groups", s.admin(http.HandlerFunc(s.scimGroupMappings)))
	mux.Handle("PUT /api/settings/scim/groups/{group}/mappings", s.admin(http.HandlerFunc(s.scimGroupMappings)))
	mux.Handle("POST /api/settings/scim/links", s.admin(http.HandlerFunc(s.scimAccountLink)))
	mux.Handle("GET /api/settings/scim/links", s.admin(http.HandlerFunc(s.scimAccountLink)))
	mux.Handle("DELETE /api/settings/scim/links/{user}", s.admin(http.HandlerFunc(s.scimAccountLink)))
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		mux.Handle(method+" /scim/v2/", s.scimAuthenticated(http.HandlerFunc(s.scimResources)))
	}
	mux.Handle("POST /api/settings/oidc/providers", s.admin(http.HandlerFunc(s.oidcProviders)))
	mux.Handle("PUT /api/settings/oidc/providers/{provider}", s.admin(http.HandlerFunc(s.oidcProviders)))
	mux.Handle("DELETE /api/settings/oidc/providers/{provider}", s.admin(http.HandlerFunc(s.oidcProviders)))
	mux.Handle("GET /api/settings/ldap/providers", s.admin(http.HandlerFunc(s.ldapProviders)))
	mux.Handle("POST /api/settings/ldap/providers", s.admin(http.HandlerFunc(s.ldapProviders)))
	mux.Handle("PUT /api/settings/ldap/providers/{provider}", s.admin(http.HandlerFunc(s.ldapProviders)))
	mux.Handle("DELETE /api/settings/ldap/providers/{provider}", s.admin(http.HandlerFunc(s.ldapProviders)))
	mux.Handle("POST /api/settings/ldap/providers/{provider}/test", s.admin(http.HandlerFunc(s.ldapTest)))
	mux.HandleFunc("GET /auth/ldap/providers", s.ldapPublicProviders)
	mux.HandleFunc("POST /auth/ldap/{provider}/sign-in", s.publicAccount(s.ldapLogin))
	mux.Handle("GET /api/settings/sign-in", s.admin(http.HandlerFunc(s.signInPolicy)))
	mux.Handle("PUT /api/settings/sign-in", s.admin(http.HandlerFunc(s.signInPolicy)))
	mux.HandleFunc("POST /auth/oidc/{provider}/start", s.publicAccount(s.oidcStart))
	mux.HandleFunc("GET /auth/oidc/{provider}/callback", s.oidcCallback)
	mux.Handle("GET /api/account/passkeys", s.admin(http.HandlerFunc(s.accountPasskeys)))
	mux.Handle("PATCH /api/account/passkeys/{key}", s.admin(http.HandlerFunc(s.accountPasskeys)))
	mux.Handle("DELETE /api/account/passkeys/{key}", s.admin(http.HandlerFunc(s.accountPasskeys)))
	mux.Handle("POST /api/account/passkeys/begin", s.admin(http.HandlerFunc(s.passkeyBegin)))
	mux.Handle("POST /api/account/passkeys/finish", s.admin(http.HandlerFunc(s.passkeyFinish)))
	mux.HandleFunc("POST /auth/passkeys/begin", s.publicAccount(s.passkeyBegin))
	mux.HandleFunc("POST /auth/passkeys/finish", s.publicAccount(s.passkeyFinish))
	mux.Handle("GET /api/account/sign-in-methods", s.admin(http.HandlerFunc(s.secondFactorStatus)))
	mux.Handle("POST /api/account/recovery-codes", s.admin(http.HandlerFunc(s.regenerateRecoveryCodes)))
	mux.Handle("DELETE /api/account/totp", s.admin(http.HandlerFunc(s.removeTOTP)))
	mux.Handle("GET /api/account/identities", s.admin(http.HandlerFunc(s.accountIdentities)))
	mux.Handle("DELETE /api/account/identities/{provider}", s.admin(http.HandlerFunc(s.unlinkIdentity)))
	mux.Handle("POST /api/account/totp", s.admin(http.HandlerFunc(s.beginTOTP)))
	mux.Handle("POST /api/account/totp/confirm", s.admin(http.HandlerFunc(s.confirmTOTP)))
	mux.HandleFunc("POST /auth/second-factor", s.publicAccount(s.secondFactorChallenge))
	mux.Handle("DELETE /api/users/{id}/second-factor", s.admin(http.HandlerFunc(s.resetSecondFactor)))
	for _, method := range []string{"GET", "PUT"} {
		mux.Handle(method+" /api/settings/second-factor", s.admin(http.HandlerFunc(s.secondFactorPolicy)))
		mux.Handle(method+" /api/teams/{id}/second-factor", s.admin(http.HandlerFunc(s.secondFactorPolicy)))
	}

	s.registerPanelRecovery(mux)
	for _, pattern := range []string{"GET /api/workloads/{id}/node-move", "POST /api/workloads/{id}/node-move", "POST /api/workloads/{id}/node-move/preview"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.workloadMove)))
	}
	mux.Handle("GET /api/nodes/{id}/gpus", s.admin(http.HandlerFunc(s.gpuInventory)))
	mux.Handle("GET /api/workloads/{id}/gpus", s.admin(http.HandlerFunc(s.gpuInventory)))
	mux.Handle("GET /api/resource-defaults", s.admin(http.HandlerFunc(s.resourceDefaults)))
	mux.Handle("PUT /api/resource-defaults", s.admin(http.HandlerFunc(s.resourceDefaults)))
	mux.Handle("GET /api/environments/{id}/resource-defaults", s.admin(http.HandlerFunc(s.resourceDefaults)))
	mux.Handle("PUT /api/environments/{id}/resource-defaults", s.admin(http.HandlerFunc(s.resourceDefaults)))
	mux.Handle("GET /api/placement-suggestions", s.admin(http.HandlerFunc(s.placementSuggestions)))
	for _, pattern := range []string{"GET /api/node-providers", "POST /api/node-providers", "PUT /api/node-providers/{id}", "DELETE /api/node-providers/{id}"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.nodeProviders)))
	}
	mux.Handle("GET /api/node-providers/{id}/catalog", s.admin(http.HandlerFunc(s.nodeProviderCatalog)))
	for _, pattern := range []string{"GET /api/node-provisions", "POST /api/node-provisions", "DELETE /api/node-provisions/{id}"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.nodeProvisions)))
	}
	mux.Handle("POST /api/node-provisions/{id}/reconcile", s.admin(http.HandlerFunc(s.reconcileNodeProvision)))
	mux.Handle("GET /api/nodes/{id}/cloud", s.admin(http.HandlerFunc(s.nodeCloudMetadata)))
	mux.Handle("GET /api/nodes/{id}/firewall", s.admin(http.HandlerFunc(s.nodeFirewall)))
	mux.Handle("POST /api/nodes/{id}/firewall/preview", s.admin(http.HandlerFunc(s.nodeFirewall)))
	mux.Handle("POST /api/nodes/{id}/firewall", s.admin(http.HandlerFunc(s.nodeFirewall)))
	mux.Handle("GET /api/nodes/{id}/host-maintenance", s.admin(http.HandlerFunc(s.nodeHostMaintenance)))
	mux.Handle("PUT /api/nodes/{id}/host-maintenance", s.admin(http.HandlerFunc(s.nodeHostMaintenance)))
	mux.HandleFunc("GET /metrics", s.prometheus)
	mux.HandleFunc("GET /api/status/{slug}", s.publicStatusPage)
	for _, pattern := range []string{"GET /api/settings/metrics-token", "POST /api/settings/metrics-token", "DELETE /api/settings/metrics-token"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.metricsToken)))
	}
	for _, pattern := range []string{"GET /api/notifications/forwarders", "POST /api/notifications/forwarders", "PUT /api/notifications/forwarders/{id}", "DELETE /api/notifications/forwarders/{id}", "POST /api/notifications/forwarders/{id}/test"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.logForwarders)))
	}
	for _, pattern := range []string{"GET /api/workloads/{id}/uptime", "POST /api/workloads/{id}/uptime", "PUT /api/workloads/{id}/uptime/{check}", "DELETE /api/workloads/{id}/uptime/{check}"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.uptimeChecks)))
	}
	for _, pattern := range []string{"GET /api/projects/{id}/status-page", "PUT /api/projects/{id}/status-page"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.statusPageSettings)))
	}
	for _, pattern := range []string{"GET /api/nodes/{id}/traffic", "POST /api/nodes/{id}/traffic"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.nodeTraffic)))
	}
	mux.Handle("GET /api/workloads/{id}/traffic", s.admin(http.HandlerFunc(s.workloadTraffic)))
	for _, pattern := range []string{"GET /api/cloudflare/connections", "POST /api/cloudflare/connections", "PUT /api/cloudflare/connections/{connection}/credentials"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.cloudflareAccounts)))
	}
	for _, pattern := range []string{"GET /api/cloudflare/connections/{connection}/zones", "POST /api/cloudflare/connections/{connection}/zones"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.cloudflareAccountZones)))
	}
	mux.Handle("GET /api/cloudflare/oauth", s.admin(http.HandlerFunc(s.cloudflareOAuthSettings)))
	mux.Handle("PUT /api/cloudflare/oauth", s.admin(http.HandlerFunc(s.cloudflareOAuthSettings)))
	mux.Handle("POST /api/cloudflare/oauth/start", s.admin(http.HandlerFunc(s.cloudflareOAuthStart)))
	mux.HandleFunc("GET /api/cloudflare/oauth/callback", s.cloudflareOAuthCallback)
	mux.Handle("PUT /api/cloudflare/{zone}/shares", s.admin(http.HandlerFunc(s.cloudflareShares)))
	mux.Handle("GET /api/cloudflare/{zone}/certificate-challenges", s.admin(http.HandlerFunc(s.certificateDNSChallenges)))
	mux.Handle("GET /api/workloads/{id}/private-network", s.admin(http.HandlerFunc(s.privateNetwork)))
	mux.Handle("PUT /api/workloads/{id}/private-network", s.admin(http.HandlerFunc(s.privateNetwork)))
	for _, pattern := range []string{"GET /api/shared-variables", "PATCH /api/shared-variables", "GET /api/projects/{id}/shared-variables", "PATCH /api/projects/{id}/shared-variables", "GET /api/environments/{id}/shared-variables", "PATCH /api/environments/{id}/shared-variables"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.sharedVariables)))
	}
	mux.Handle("POST /api/projects/{id}/deployments", s.admin(http.HandlerFunc(s.queueDeploymentPlan)))
	mux.Handle("GET /api/projects/{id}/deployment-sequence", s.admin(http.HandlerFunc(s.deploymentSequence)))
	mux.Handle("PUT /api/projects/{id}/deployment-sequence", s.admin(http.HandlerFunc(s.deploymentSequence)))
	mux.Handle("GET /api/cloudflare", s.admin(http.HandlerFunc(s.cloudflareSettings)))
	mux.Handle("PUT /api/cloudflare", s.admin(http.HandlerFunc(s.cloudflareSettings)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if s.DB.Ping(ctx) != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		respond(w, 200, map[string]string{"status": "ok"})
	})
	for _, pattern := range []string{"GET /api/secret-providers", "POST /api/secret-providers", "PUT /api/secret-providers/{connection}", "DELETE /api/secret-providers/{connection}"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.secretProviders)))
	}
	for _, pattern := range []string{"GET /api/projects/{id}/configuration/gitops", "PUT /api/projects/{id}/configuration/gitops", "DELETE /api/projects/{id}/configuration/gitops"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.projectGitOps)))
	}
	mux.Handle("POST /api/projects/{id}/configuration/gitops/sync", s.admin(http.HandlerFunc(s.syncProjectGitOps)))
	mux.Handle("GET /api/workloads/{id}/configuration-diff", s.admin(http.HandlerFunc(s.workloadConfigurationDiff)))
	mux.Handle("GET /api/workloads/{id}/drift", s.admin(http.HandlerFunc(s.workloadDrift)))
	mux.Handle("GET /api/projects/{id}/configuration/compare", s.admin(http.HandlerFunc(s.compareEnvironments)))
	mux.Handle("POST /api/projects/{id}/configuration/export", s.admin(http.HandlerFunc(s.exportProjectConfiguration)))
	mux.Handle("GET /api/projects/{id}/configuration", s.admin(http.HandlerFunc(s.projectConfigurationHandler)))
	for _, pattern := range []string{"GET /api/projects/{id}/configuration/drafts", "POST /api/projects/{id}/configuration/drafts", "DELETE /api/projects/{id}/configuration/drafts/{draft}"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.configurationDrafts)))
	}
	mux.Handle("POST /api/projects/{id}/configuration/drafts/{draft}/apply", s.admin(http.HandlerFunc(s.applyConfigurationDraft)))
	for _, pattern := range []string{"GET /api/projects/{id}/imports", "POST /api/projects/{id}/imports/coolify", "GET /api/projects/{id}/imports/{import}", "DELETE /api/projects/{id}/imports/{import}", "POST /api/projects/{id}/imports/{import}/dry-run"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.migrationImports)))
	}
	for _, pattern := range []string{"GET /api/workloads/{id}/compose-bundle", "PUT /api/workloads/{id}/compose-bundle", "POST /api/workloads/{id}/compose-bundle/export"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.composeBundle)))
	}
	mux.Handle("POST /api/workloads/{id}/variables/import", s.admin(http.HandlerFunc(s.environmentFile)))
	mux.Handle("POST /api/workloads/{id}/variables/export", s.admin(http.HandlerFunc(s.environmentFile)))
	mux.Handle("GET /api/workloads/{id}/variables/builtins", s.admin(http.HandlerFunc(s.builtinVariables)))
	mux.Handle("GET /api/git/connections", s.admin(http.HandlerFunc(s.gitConnections)))
	mux.Handle("POST /api/git/connections", s.admin(http.HandlerFunc(s.gitConnections)))
	mux.Handle("DELETE /api/git/connections/{connection}", s.admin(http.HandlerFunc(s.gitConnections)))
	mux.Handle("GET /api/git/connections/{connection}/repositories", s.admin(http.HandlerFunc(s.gitProviderRepositories)))
	mux.Handle("GET /api/git/connections/{connection}/repositories/{repository}/branches", s.admin(http.HandlerFunc(s.gitProviderRepositories)))
	mux.Handle("GET /api/workloads/{id}/git-provider", s.admin(http.HandlerFunc(s.gitProviderSource)))
	mux.Handle("PUT /api/workloads/{id}/git-provider", s.admin(http.HandlerFunc(s.gitProviderSource)))
	mux.Handle("DELETE /api/workloads/{id}/git-provider", s.admin(http.HandlerFunc(s.gitProviderSource)))
	mux.HandleFunc("POST /hooks/git/{connection}", s.gitProviderWebhook)
	mux.Handle("GET /api/github/app", s.admin(http.HandlerFunc(s.githubAppInfo)))
	mux.Handle("PUT /api/github/app/user-auth", s.admin(http.HandlerFunc(s.githubAppUserAuth)))
	mux.Handle("POST /api/github/app/connect", s.admin(http.HandlerFunc(s.githubInstallationStart)))
	mux.HandleFunc("GET /api/github/app/connect/callback", s.githubInstallationCallback)
	mux.Handle("DELETE /api/github/app/installations/{installation}", s.admin(http.HandlerFunc(s.disconnectGithubInstallation)))
	mux.Handle("POST /api/github/app/setup", s.admin(http.HandlerFunc(s.githubAppSetup)))
	mux.HandleFunc("GET /api/github/app/callback", s.githubAppCallback)
	mux.Handle("POST /api/github/app/sync", s.admin(http.HandlerFunc(s.syncGithubInstallations)))
	mux.Handle("PUT /api/github/app/installations/{installation}", s.admin(http.HandlerFunc(s.assignGithubInstallation)))
	mux.Handle("GET /api/github/repositories", s.admin(http.HandlerFunc(s.githubRepositories)))
	mux.Handle("GET /api/workloads/{id}/deploy-hook", s.admin(http.HandlerFunc(s.deployHookSettings)))
	mux.Handle("POST /api/workloads/{id}/deploy-hook", s.admin(http.HandlerFunc(s.deployHookSettings)))
	mux.Handle("DELETE /api/workloads/{id}/deploy-hook", s.admin(http.HandlerFunc(s.deployHookSettings)))
	mux.HandleFunc("POST /hooks/deploy/{hook}", s.inboundDeployHook)
	mux.HandleFunc("GET /hooks/deploy/{hook}/deliveries/{delivery}", s.inboundDeployHook)
	mux.Handle("GET /api/workloads/{id}/git-trigger", s.admin(http.HandlerFunc(s.gitSourceTrigger)))
	mux.Handle("PUT /api/workloads/{id}/git-trigger", s.admin(http.HandlerFunc(s.gitSourceTrigger)))
	mux.Handle("GET /api/workloads/{id}/git-deploy-key", s.admin(http.HandlerFunc(s.gitDeployKey)))
	mux.Handle("POST /api/workloads/{id}/git-deploy-key", s.admin(http.HandlerFunc(s.gitDeployKey)))
	mux.Handle("DELETE /api/workloads/{id}/git-deploy-key", s.admin(http.HandlerFunc(s.gitDeployKey)))
	mux.Handle("GET /api/workloads/{id}/github/reporting", s.admin(http.HandlerFunc(s.githubReporting)))
	mux.Handle("PUT /api/workloads/{id}/github/reporting", s.admin(http.HandlerFunc(s.githubReporting)))
	mux.Handle("GET /api/workloads/{id}/github", s.admin(http.HandlerFunc(s.githubSource)))
	for _, method := range []string{"GET", "PUT", "POST", "DELETE"} {
		mux.Handle(method+" /api/workloads/{id}/previews", s.admin(http.HandlerFunc(s.previews)))
	}
	mux.Handle("GET /api/projects/{id}/preview-recipe", s.admin(http.HandlerFunc(s.previewRecipeHandler)))
	mux.Handle("PUT /api/projects/{id}/preview-recipe", s.admin(http.HandlerFunc(s.previewRecipeHandler)))
	mux.Handle("GET /api/projects/{id}/environment-previews", s.admin(http.HandlerFunc(s.environmentPreviews)))
	mux.Handle("POST /api/projects/{id}/environment-previews", s.admin(http.HandlerFunc(s.environmentPreviews)))
	for _, pattern := range []string{"GET /api/environment-previews/{id}", "PUT /api/environment-previews/{id}", "DELETE /api/environment-previews/{id}", "GET /api/environment-previews/{id}/{action}", "POST /api/environment-previews/{id}/{action}"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.environmentPreview)))
	}
	mux.Handle("PUT /api/workloads/{id}/github", s.admin(http.HandlerFunc(s.githubSource)))
	mux.Handle("DELETE /api/workloads/{id}/github", s.admin(http.HandlerFunc(s.githubSource)))
	mux.HandleFunc("POST /hooks/github-app", s.githubAppWebhook)
	mux.Handle("POST /api/nodes", s.admin(http.HandlerFunc(s.createNode)))
	mux.Handle("POST /api/nodes/local", s.admin(http.HandlerFunc(s.addLocalNode)))
	mux.Handle("GET /api/networks", s.admin(http.HandlerFunc(s.sharedNetworks)))
	mux.Handle("POST /api/networks", s.admin(http.HandlerFunc(s.sharedNetworks)))
	mux.Handle("GET /api/workloads/{id}/networks", s.admin(http.HandlerFunc(s.workloadNetworks)))
	mux.Handle("PUT /api/workloads/{id}/networks", s.admin(http.HandlerFunc(s.workloadNetworks)))
	mux.Handle("GET /api/notifications/health-history", s.admin(http.HandlerFunc(s.healthDeliveryHistory)))
	mux.Handle("GET /api/notifications/health", s.admin(http.HandlerFunc(s.healthAlerts)))
	mux.Handle("GET /api/nodes", s.admin(http.HandlerFunc(s.listNodes)))
	mux.Handle("GET /api/nodes/{id}/cleanup-policy", s.admin(http.HandlerFunc(s.nodeCleanupControl)))
	mux.Handle("PUT /api/nodes/{id}/cleanup-policy", s.admin(http.HandlerFunc(s.nodeCleanupControl)))
	mux.Handle("POST /api/nodes/{id}/cleanup-plan", s.admin(http.HandlerFunc(s.nodeCleanupControl)))
	mux.Handle("POST /api/nodes/{id}/cleanup-run", s.admin(http.HandlerFunc(s.nodeCleanupControl)))
	mux.Handle("GET /api/notifications/mounts", s.admin(http.HandlerFunc(s.notificationMounts)))
	mux.Handle("GET /api/settings/operation-retention", s.admin(http.HandlerFunc(s.operationRetentionStatus)))
	mux.Handle("POST /api/settings/operation-retention/run", s.admin(http.HandlerFunc(s.operationRetentionStatus)))
	mux.Handle("GET /api/nodes/{id}/cleanup-preview", s.admin(http.HandlerFunc(s.cleanupPreview)))
	mux.Handle("POST /api/nodes/{id}/verify", s.admin(http.HandlerFunc(s.verifyNode)))
	mux.Handle("PUT /api/nodes/{id}", s.admin(http.HandlerFunc(s.updateNode)))
	mux.Handle("DELETE /api/nodes/{id}", s.admin(http.HandlerFunc(s.deleteNode)))
	mux.Handle("POST /api/nodes/{id}/enrollment", s.admin(http.HandlerFunc(s.renewEnrollment)))
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		mux.Handle(method+" /api/nodes/{id}/cloudflare-tunnel", s.admin(http.HandlerFunc(s.nodeCloudflareTunnel)))
	}
	mux.Handle("POST /api/nodes/{id}/cloudflare-tunnel/retry", s.admin(http.HandlerFunc(s.nodeCloudflareTunnel)))
	mux.Handle("GET /api/nodes/{id}/ingress-listeners", s.admin(http.HandlerFunc(s.ingressExposures)))
	mux.Handle("POST /api/nodes/{id}/ingress-listeners/initialize", s.admin(http.HandlerFunc(s.initializeIngressExposures)))
	mux.Handle("POST /api/nodes/{id}/ingress-listeners/plan", s.admin(http.HandlerFunc(s.planIngressExposures)))
	mux.Handle("PUT /api/nodes/{id}/ingress-listeners", s.admin(http.HandlerFunc(s.changeIngressExposures)))
	mux.Handle("GET /api/nodes/{id}/ingress-config", s.admin(http.HandlerFunc(s.ingressConfig)))
	mux.Handle("POST /api/nodes/{id}/ingress-config/validate", s.admin(http.HandlerFunc(s.validateIngressConfig)))
	mux.Handle("GET /api/nodes/{id}/ingress-config/files/{name}", s.admin(http.HandlerFunc(s.ingressConfigFile)))
	mux.Handle("PUT /api/nodes/{id}/ingress-config/files/{name}", s.admin(http.HandlerFunc(s.putIngressConfigFile)))
	mux.Handle("POST /api/nodes/{id}/ingress-config/files/{name}/import", s.admin(http.HandlerFunc(s.importIngressConfigFile)))
	mux.Handle("DELETE /api/nodes/{id}/ingress-config/files/{name}", s.admin(http.HandlerFunc(s.deleteIngressConfigFile)))
	for _, method := range []string{"GET", "POST", "DELETE"} {
		mux.Handle(method+" /api/workloads/{id}/database-access", s.admin(http.HandlerFunc(s.databaseAccess)))
	}
	mux.Handle("POST /api/workloads/{id}/restore-copy", s.admin(http.HandlerFunc(s.restoreCopy)))
	mux.Handle("GET /api/workloads", s.admin(http.HandlerFunc(s.listWorkloads)))
	mux.Handle("POST /api/projects/{id}/clone", s.admin(http.HandlerFunc(s.clone)))
	mux.Handle("POST /api/environments/{id}/clone", s.admin(http.HandlerFunc(s.clone)))
	mux.Handle("POST /api/workloads/{id}/clone", s.admin(http.HandlerFunc(s.clone)))
	mux.Handle("GET /api/projects", s.admin(http.HandlerFunc(s.listProjects)))
	mux.Handle("POST /api/projects", s.admin(http.HandlerFunc(s.createProject)))
	mux.Handle("POST /api/projects/{id}/environments", s.admin(http.HandlerFunc(s.createEnvironment)))
	mux.Handle("DELETE /api/projects/{id}", s.admin(http.HandlerFunc(s.deleteProject)))
	mux.Handle("DELETE /api/environments/{id}", s.admin(http.HandlerFunc(s.deleteEnvironment)))
	mux.Handle("GET /api/environments/{id}/suspension", s.admin(http.HandlerFunc(s.environmentSuspension)))
	mux.Handle("PUT /api/environments/{id}/suspension", s.admin(http.HandlerFunc(s.environmentSuspension)))
	mux.Handle("PUT /api/workloads/{id}/environment", s.admin(http.HandlerFunc(s.assignEnvironment)))
	mux.Handle("POST /api/workloads/{id}/move/preview", s.admin(http.HandlerFunc(s.moveResource)))
	mux.Handle("POST /api/workloads/{id}/move", s.admin(http.HandlerFunc(s.moveResource)))
	for _, pattern := range []string{"GET /api/nodes/{id}/adoptions", "POST /api/nodes/{id}/adoptions/discover", "POST /api/nodes/{id}/adoptions", "GET /api/nodes/{id}/adoptions/{adoption}", "DELETE /api/nodes/{id}/adoptions/{adoption}", "POST /api/nodes/{id}/adoptions/{adoption}/adopt", "POST /api/nodes/{id}/adoptions/{adoption}/refresh"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.composeAdoptions)))
	}
	mux.Handle("POST /api/workloads", s.admin(http.HandlerFunc(s.createWorkload)))
	mux.Handle("GET /api/workloads/{id}", s.admin(http.HandlerFunc(s.getWorkload)))
	mux.Handle("PUT /api/workloads/{id}", s.admin(http.HandlerFunc(s.updateWorkload)))
	for _, method := range []string{"GET", "POST"} {
		mux.Handle(method+" /api/workloads/{id}/certificates", s.admin(http.HandlerFunc(s.routeCertificates)))
	}
	mux.Handle("DELETE /api/workloads/{id}/certificates/{certificate}", s.admin(http.HandlerFunc(s.routeCertificates)))
	mux.Handle("GET /api/workloads/{id}/route-listeners", s.admin(http.HandlerFunc(s.routeListeners)))
	mux.Handle("GET /api/workloads/{id}/certificate-zones", s.admin(http.HandlerFunc(s.certificateZones)))
	mux.Handle("GET /api/workloads/{id}/certificate-authority", s.admin(http.HandlerFunc(s.certificateAuthority)))
	mux.Handle("GET /api/workloads/{id}/certificate-challenges", s.admin(http.HandlerFunc(s.certificateDNSChallenges)))
	mux.Handle("GET /api/workloads/{id}/certificate-issuers", s.admin(http.HandlerFunc(s.certificateIssuers)))
	mux.Handle("POST /api/workloads/{id}/certificate-issuers", s.admin(http.HandlerFunc(s.certificateIssuers)))
	mux.Handle("PUT /api/workloads/{id}/certificate-issuers/{issuer}", s.admin(http.HandlerFunc(s.updateCertificateIssuer)))
	mux.Handle("DELETE /api/workloads/{id}/certificate-issuers/{issuer}", s.admin(http.HandlerFunc(s.updateCertificateIssuer)))
	mux.Handle("PUT /api/workloads/{id}/routes", s.admin(http.HandlerFunc(s.updateRoutes)))
	mux.Handle("DELETE /api/workloads/{id}", s.admin(http.HandlerFunc(s.deleteWorkload)))
	mux.Handle("GET /api/workloads/{id}/runtime", s.admin(http.HandlerFunc(s.workloadRuntime)))
	mux.Handle("GET /api/workloads/{id}/logs", s.admin(http.HandlerFunc(s.workloadLogs)))
	mux.Handle("GET /api/workloads/{id}/backups", s.admin(http.HandlerFunc(s.workloadBackups)))
	mux.HandleFunc("GET /api/workloads/{id}/backups/{backup}/download", s.nativeBackupDownload)
	mux.Handle("POST /api/workloads/{id}/backups/import", s.admin(http.HandlerFunc(s.importBackupDump)))
	mux.Handle("POST /api/workloads/{id}/backups/import-s3", s.admin(http.HandlerFunc(s.importBackupDump)))
	mux.Handle("GET /api/settings/transfer-limits", s.admin(http.HandlerFunc(s.transferLimits)))
	mux.Handle("GET /api/workloads/{id}/backups/transfer-limits", s.admin(http.HandlerFunc(s.transferLimits)))
	mux.Handle("PUT /api/settings/transfer-limits", s.admin(http.HandlerFunc(s.transferLimits)))
	mux.Handle("GET /api/workloads/{id}/releases", s.admin(http.HandlerFunc(s.workloadReleases)))
	mux.Handle("GET /api/workloads/{id}/build-node", s.admin(http.HandlerFunc(s.buildNode)))
	mux.Handle("PUT /api/workloads/{id}/build-node", s.admin(http.HandlerFunc(s.buildNode)))
	mux.Handle("GET /api/workloads/{id}/images", s.admin(http.HandlerFunc(s.workloadImages)))
	mux.Handle("GET /api/workloads/{id}/image-scanning", s.admin(http.HandlerFunc(s.imageScanning)))
	mux.Handle("PUT /api/workloads/{id}/image-scanning", s.admin(http.HandlerFunc(s.imageScanning)))
	mux.Handle("GET /api/workloads/{id}/image-scans", s.admin(http.HandlerFunc(s.workloadImageScans)))
	mux.Handle("GET /api/workloads/{id}/backup-policy", s.admin(http.HandlerFunc(s.getBackupPolicy)))
	mux.Handle("GET /api/workloads/{id}/pitr", s.admin(http.HandlerFunc(s.getPITR)))
	mux.Handle("PUT /api/workloads/{id}/pitr", s.admin(http.HandlerFunc(s.changePITR)))
	mux.Handle("DELETE /api/workloads/{id}/pitr", s.admin(http.HandlerFunc(s.changePITR)))
	mux.Handle("POST /api/workloads/{id}/pitr/backup", s.admin(http.HandlerFunc(s.changePITR)))
	mux.Handle("POST /api/workloads/{id}/pitr/restore-copy", s.admin(http.HandlerFunc(s.restorePITR)))
	mux.Handle("GET /api/workloads/{id}/backups/snapshot-preview", s.admin(http.HandlerFunc(s.backupSnapshotPreview)))
	mux.Handle("PUT /api/workloads/{id}/backup-policy", s.admin(http.HandlerFunc(s.setBackupPolicy)))
	mux.Handle("GET /api/workloads/{id}/webhook", s.admin(http.HandlerFunc(s.getWebhook)))
	mux.Handle("POST /api/workloads/{id}/webhook", s.admin(http.HandlerFunc(s.createWebhook)))
	mux.Handle("DELETE /api/workloads/{id}/webhook", s.admin(http.HandlerFunc(s.deleteWebhook)))
	mux.HandleFunc("POST /hooks/github/{id}", s.githubWebhook)
	mux.Handle("POST /api/workloads/{id}/operations", s.admin(http.HandlerFunc(s.queueOperation)))
	mux.Handle("GET /api/operations", s.admin(http.HandlerFunc(s.listOperations)))
	mux.Handle("GET /api/operations/{id}", s.admin(http.HandlerFunc(s.getOperation)))
	mux.Handle("GET /api/operations/{id}/logs", s.admin(http.HandlerFunc(s.operationLogs)))
	mux.Handle("POST /api/operations/{id}/cancel", s.admin(http.HandlerFunc(s.cancelOperation)))
	mux.HandleFunc("POST /auth/login", func(w http.ResponseWriter, r *http.Request) {
		s.authenticated(http.HandlerFunc(s.loginAttempt(s.login)), w, r, "browser authentication")
	})
	mux.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		s.authenticated(http.HandlerFunc(s.logout), w, r, "browser session")
	})
	mux.Handle("GET /api/session", s.admin(http.HandlerFunc(s.sessionInfo)))
	mux.Handle("GET /api/audit", s.admin(http.HandlerFunc(s.auditEvents)))
	mux.Handle("POST /api/password", s.admin(http.HandlerFunc(s.changePassword)))
	mux.Handle("GET /api/tokens", s.admin(http.HandlerFunc(s.listTokens)))
	mux.Handle("POST /api/tokens", s.admin(http.HandlerFunc(s.createToken)))
	mux.Handle("DELETE /api/tokens/{id}", s.admin(http.HandlerFunc(s.revokeToken)))
	mux.HandleFunc("GET /auth/config", s.authConfig)
	mux.HandleFunc("POST /auth/register", s.publicAccount(s.registerAccount))
	mux.HandleFunc("POST /auth/verify", s.publicAccount(s.verifyAccount))
	mux.HandleFunc("POST /auth/forgot-password", s.publicAccount(s.forgotPassword))
	mux.HandleFunc("POST /auth/reset-password", s.publicAccount(s.resetPassword))
	mux.HandleFunc("POST /auth/invitation", s.publicAccount(s.previewInvitation))
	mux.HandleFunc("POST /auth/accept-invitation", s.publicAccount(s.acceptInvitation))
	mux.HandleFunc("POST /auth/github/start", s.publicAccount(s.githubStart))
	mux.HandleFunc("GET /auth/github/callback", s.githubCallback)
	mux.Handle("GET /api/teams", s.admin(http.HandlerFunc(s.listTeams)))
	mux.Handle("POST /api/teams", s.admin(http.HandlerFunc(s.createTeam)))
	mux.Handle("GET /api/teams/{id}/members", s.admin(http.HandlerFunc(s.listMembers)))
	mux.Handle("PUT /api/teams/{id}/members/{user}", s.admin(http.HandlerFunc(s.changeMember)))
	mux.Handle("DELETE /api/teams/{id}/members/{user}", s.admin(http.HandlerFunc(s.changeMember)))
	mux.Handle("GET /api/teams/{id}/invitations", s.admin(http.HandlerFunc(s.invitations)))
	mux.Handle("POST /api/teams/{id}/invitations", s.admin(http.HandlerFunc(s.invitations)))
	mux.Handle("DELETE /api/teams/{id}/invitations/{invite}", s.admin(http.HandlerFunc(s.invitations)))
	mux.Handle("GET /api/teams/{id}/nodes", s.admin(http.HandlerFunc(s.teamNodes)))
	mux.Handle("PUT /api/teams/{id}/nodes", s.admin(http.HandlerFunc(s.teamNodes)))
	mux.Handle("POST /api/settings/smtp/test", s.admin(http.HandlerFunc(s.testEmail)))
	mux.Handle("GET /api/settings/integrations", s.admin(http.HandlerFunc(s.integrations)))
	mux.Handle("PUT /api/settings/integrations", s.admin(http.HandlerFunc(s.integrations)))
	mux.Handle("GET /api/settings", s.admin(http.HandlerFunc(s.settings)))
	mux.Handle("GET /api/updates", s.admin(http.HandlerFunc(s.updateSettings)))
	mux.Handle("PUT /api/updates", s.admin(http.HandlerFunc(s.updateSettings)))
	mux.Handle("PUT /api/settings", s.admin(http.HandlerFunc(s.settings)))
	mux.Handle("GET /api/users", s.admin(http.HandlerFunc(s.users)))
	mux.Handle("PUT /api/users/{id}", s.admin(http.HandlerFunc(s.users)))
	mux.Handle("POST /api/users/{id}/reset", s.admin(http.HandlerFunc(s.users)))
	mux.Handle("GET /api/account/sessions", s.admin(http.HandlerFunc(s.listAccountSessions)))
	mux.Handle("DELETE /api/account/sessions/{id}", s.admin(http.HandlerFunc(s.revokeAccountSession)))
	mux.Handle("POST /api/workloads/{id}/preflight", s.admin(http.HandlerFunc(s.preflight)))
	mux.Handle("POST /api/workloads/{id}/promote", s.admin(http.HandlerFunc(s.promoteRelease)))
	for _, scope := range []string{"projects", "environments"} {
		mux.Handle("GET /api/"+scope+"/{id}/deployment-policy", s.admin(http.HandlerFunc(s.deploymentPolicy)))
		mux.Handle("PUT /api/"+scope+"/{id}/deployment-policy", s.admin(http.HandlerFunc(s.deploymentPolicy)))
	}
	mux.Handle("POST /api/operations/{id}/approve", s.admin(http.HandlerFunc(s.approveDeployment)))
	mux.Handle("GET /api/teams/{id}/quota", s.admin(http.HandlerFunc(s.teamQuota)))
	mux.Handle("PUT /api/teams/{id}/quota", s.admin(http.HandlerFunc(s.teamQuota)))
	mux.Handle("GET /api/projects/{id}/dependencies", s.admin(http.HandlerFunc(s.dependencyMap)))
	mux.Handle("GET /api/projects/{id}/connections", s.admin(http.HandlerFunc(s.projectConnections)))
	mux.Handle("POST /api/private-connections", s.admin(http.HandlerFunc(s.createPrivateConnection)))
	mux.Handle("PUT /api/private-connections/{id}", s.admin(http.HandlerFunc(s.updatePrivateConnection)))
	mux.Handle("DELETE /api/private-connections/{id}", s.admin(http.HandlerFunc(s.deletePrivateConnection)))
	mux.Handle("POST /api/private-connections/{id}/retry", s.admin(http.HandlerFunc(s.retryPrivateConnection)))
	mux.Handle("GET /api/nodes/{id}/private-links", s.admin(http.HandlerFunc(s.nodePrivateLinks)))
	mux.Handle("PUT /api/nodes/{id}/private-links", s.admin(http.HandlerFunc(s.nodePrivateLinks)))
	mux.Handle("GET /api/workloads/{id}/schedule", s.admin(http.HandlerFunc(s.jobSchedule)))
	mux.Handle("PUT /api/workloads/{id}/schedule", s.admin(http.HandlerFunc(s.jobSchedule)))
	mux.Handle("GET /api/workloads/{id}/autoscale-policy", s.admin(http.HandlerFunc(s.autoscalePolicy)))
	mux.Handle("PUT /api/workloads/{id}/autoscale-policy", s.admin(http.HandlerFunc(s.autoscalePolicy)))
	mux.Handle("GET /api/workloads/{id}/idle-policy", s.admin(http.HandlerFunc(s.idlePolicy)))
	mux.Handle("PUT /api/workloads/{id}/idle-policy", s.admin(http.HandlerFunc(s.idlePolicy)))
	mux.Handle("GET /api/workloads/{id}/restart-policy", s.admin(http.HandlerFunc(s.unhealthyRestartPolicy)))
	mux.Handle("PUT /api/workloads/{id}/restart-policy", s.admin(http.HandlerFunc(s.unhealthyRestartPolicy)))
	mux.Handle("GET /api/workloads/{id}/configuration-history", s.admin(http.HandlerFunc(s.configurationHistory)))
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		mux.Handle(method+" /api/workloads/{id}/variable-bindings", s.admin(http.HandlerFunc(s.resourceVariables)))
	}
	mux.Handle("PUT /api/account", s.admin(http.HandlerFunc(s.updateAccount)))
	for _, prefix := range []string{"/api/registries", "/api/storage-locations", "/api/notifications"} {
		mux.Handle("GET "+prefix, s.admin(http.HandlerFunc(s.connections)))
		mux.Handle("POST "+prefix, s.admin(http.HandlerFunc(s.connections)))
		mux.Handle("PUT "+prefix+"/{id}", s.admin(http.HandlerFunc(s.connections)))
		mux.Handle("DELETE "+prefix+"/{id}", s.admin(http.HandlerFunc(s.connections)))
		mux.Handle("POST "+prefix+"/{id}/test", s.admin(http.HandlerFunc(s.testConnection)))
	}
	for _, pattern := range []string{"GET /api/managed-registries", "POST /api/managed-registries", "PUT /api/managed-registries/{id}", "POST /api/managed-registries/{id}/retry", "POST /api/managed-registries/{id}/gc", "DELETE /api/managed-registries/{id}"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.managedRegistries)))
	}
	mux.Handle("DELETE /api/managed-registries/{id}/record", s.admin(http.HandlerFunc(s.deleteManagedRegistryRecord)))
	mux.Handle("GET /api/notifications/history", s.admin(http.HandlerFunc(s.notificationHistory)))
	mux.Handle("GET /api/notifications/events", s.admin(http.HandlerFunc(s.notificationCatalogue)))
	mux.Handle("GET /api/workloads/{id}/metrics", s.admin(http.HandlerFunc(s.metricHistory)))
	mux.Handle("GET /api/nodes/{id}/metrics", s.admin(http.HandlerFunc(s.metricHistory)))
	mux.Handle("GET /api/workloads/{id}/log-history", s.admin(http.HandlerFunc(s.logHistory)))
	mux.Handle("GET /api/settings/retention", s.admin(http.HandlerFunc(s.telemetryRetention)))
	mux.Handle("PUT /api/settings/retention", s.admin(http.HandlerFunc(s.telemetryRetention)))
	mux.Handle("GET /api/settings/storage-usage", s.admin(http.HandlerFunc(s.telemetryStorageUsage)))
	mux.Handle("GET /api/notifications/rules", s.admin(http.HandlerFunc(s.thresholdRules)))
	mux.Handle("POST /api/notifications/rules", s.admin(http.HandlerFunc(s.thresholdRules)))
	mux.Handle("DELETE /api/notifications/rules/{id}", s.admin(http.HandlerFunc(s.thresholdRules)))
	mux.Handle("GET /api/workloads/{id}/owner", s.admin(http.HandlerFunc(s.workloadOwner)))
	mux.Handle("GET /api/workloads/{id}/capacity", s.admin(http.HandlerFunc(s.workloadCapacity)))
	mux.Handle("GET /api/workloads/{id}/sizing", s.admin(http.HandlerFunc(s.workloadSizing)))
	mux.Handle("GET /api/workloads/{id}/logs/live", s.admin(http.HandlerFunc(s.workloadLiveLogs)))
	mux.Handle("GET /api/nodes/{id}/cost", s.admin(http.HandlerFunc(s.nodeCost)))
	mux.Handle("PUT /api/nodes/{id}/cost", s.admin(http.HandlerFunc(s.nodeCost)))
	mux.Handle("GET /api/projects/{id}/costs", s.admin(http.HandlerFunc(s.projectCosts)))
	mux.Handle("GET /api/workloads/{id}/observations", s.admin(http.HandlerFunc(s.workloadObservations)))
	mux.Handle("GET /api/workloads/{id}/files", s.admin(http.HandlerFunc(s.workloadFiles)))
	mux.Handle("GET /api/bastions", s.admin(http.HandlerFunc(s.bastions)))
	mux.Handle("GET /api/nodes/{id}/identity", s.admin(http.HandlerFunc(s.nodeIdentity)))
	mux.Handle("POST /api/nodes/{id}/identity", s.admin(http.HandlerFunc(s.nodeIdentity)))
	mux.Handle("POST /api/bastions", s.admin(http.HandlerFunc(s.bastions)))
	mux.Handle("DELETE /api/bastions/{id}", s.admin(http.HandlerFunc(s.bastions)))
	mux.Handle("GET /api/nodes/{id}/bastion", s.admin(http.HandlerFunc(s.nodeBastion)))
	mux.Handle("PUT /api/nodes/{id}/bastion", s.admin(http.HandlerFunc(s.nodeBastion)))
	mux.Handle("GET /api/workloads/{id}/files/targets", s.admin(http.HandlerFunc(s.workloadFiles)))
	mux.Handle("GET /api/workloads/{id}/files/content", s.admin(http.HandlerFunc(s.workloadFiles)))
	mux.Handle("GET /api/workloads/{id}/files/metadata", s.admin(http.HandlerFunc(s.workloadFiles)))
	mux.Handle("GET /api/workloads/{id}/files/limits", s.admin(http.HandlerFunc(s.fileTransferLimits)))
	mux.Handle("GET /api/settings/file-transfer-limits", s.admin(http.HandlerFunc(s.fileTransferLimits)))
	mux.Handle("PUT /api/settings/file-transfer-limits", s.admin(http.HandlerFunc(s.fileTransferLimits)))
	mux.HandleFunc("GET /api/workloads/{id}/files/download", s.nativeFileDownload)
	mux.Handle("PUT /api/workloads/{id}/files/content", s.admin(http.HandlerFunc(s.workloadFiles)))
	mux.Handle("POST /api/workloads/{id}/files/upload", s.admin(http.HandlerFunc(s.workloadFiles)))
	mux.Handle("DELETE /api/workloads/{id}/files/content", s.admin(http.HandlerFunc(s.workloadFiles)))
	mux.Handle("PUT /api/workloads/{id}/owner", s.admin(http.HandlerFunc(s.workloadOwner)))
	mux.Handle("GET /api/workloads/{id}/registry", s.admin(http.HandlerFunc(s.resourceRegistry)))
	mux.Handle("PUT /api/workloads/{id}/registry", s.admin(http.HandlerFunc(s.resourceRegistry)))
	for _, pattern := range []string{"GET /api/workloads/{id}/registry-webhook", "PUT /api/workloads/{id}/registry-webhook", "DELETE /api/workloads/{id}/registry-webhook", "POST /api/workloads/{id}/registry-webhook/rotate"} {
		mux.Handle(pattern, s.admin(http.HandlerFunc(s.registryWebhook)))
	}
	mux.HandleFunc("POST /hooks/registry/{hook}", s.receiveRegistryWebhook)
	for _, method := range []string{"GET", "PUT"} {
		mux.Handle(method+" /api/workloads/{id}/image-update-policy", s.admin(http.HandlerFunc(s.imageUpdatePolicy)))
	}
	mux.Handle("POST /api/terminal", s.admin(http.HandlerFunc(s.terminalOpen)))
	mux.Handle("GET /api/terminal/{id}", s.admin(http.HandlerFunc(s.terminalSession)))
	mux.Handle("POST /api/terminal/{id}/input", s.admin(http.HandlerFunc(s.terminalSession)))
	mux.Handle("DELETE /api/terminal/{id}", s.admin(http.HandlerFunc(s.terminalSession)))
	mux.HandleFunc("GET /install.sh", s.install)
	mux.HandleFunc("GET /releases/worker/{arch}", s.workerRelease)
	mux.HandleFunc("GET /releases/worker/{arch}/{checksum}/signature", s.workerSignature)
	mux.HandleFunc("GET /releases/worker/install.sh", s.workerUpdateInstaller)
	mux.HandleFunc("GET /releases/worker/{arch}/{checksum}", s.workerRelease)
	mux.HandleFunc("GET /bootstrap/worker/{arch}", s.binary)
	mux.HandleFunc("POST /bootstrap/register", func(w http.ResponseWriter, r *http.Request) {
		s.authenticated(http.HandlerFunc(s.register), w, r, "node enrollment")
	})
	mux.HandleFunc("GET /", s.frontend)
	mounted := s.mountPanel(s.restrictPanel(mux))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mounted.ServeHTTP(w, r)
	})
}
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}
func (s *Server) admin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := &statusResponse{ResponseWriter: w}
		w = response
		defer func() {
			if response.status == 401 || response.status == 403 {
				s.recordAudit(r, "rejected authentication or authorization", response.status)
			}
		}()
		a, b := sha256.Sum256([]byte(bearer(r))), sha256.Sum256([]byte(s.AdminToken))
		if subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
			if bearer(r) != "" {
				s.scopedToken(next, w, r)
				return
			}
			p, err := s.sessionIdentity(r)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "authentication unavailable", 503)
				return
			}
			if err == nil && (r.Method == "GET" || s.sameOrigin(r)) {
				r = r.WithContext(context.WithValue(r.Context(), identityKey{}, p))
				s.serveAuthorized(next, w, r, "user "+p.ID)
				return
			}
			http.Error(w, "unauthorized", 401)
			return
		}
		p, err := s.loadIdentity(r, "bootstrap", true)
		if err != nil {
			http.Error(w, "authentication unavailable", 503)
			return
		}
		p.Admin = true
		r = r.WithContext(context.WithValue(r.Context(), identityKey{}, p))
		s.serveAuthorized(next, w, r, "administrator API token")
	})
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2*1024*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		http.Error(w, "invalid request", 400)
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(w, "invalid request", 400)
		return false
	}
	return true
}

func (s *Server) createNode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string `json:"name"`
		Address string `json:"address"`
		Port    int    `json:"port"`
	}
	if !decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 120 || in.Address == "" || len(in.Address) > 253 || strings.ContainsAny(in.Address, " \t\r\n/\\\"'") || in.Port < 1 || in.Port > 65535 {
		http.Error(w, "name, SSH address, and valid port required", 400)
		return
	}
	pub, priv, err := keyPair()
	if err != nil {
		http.Error(w, "key generation failed", 500)
		return
	}
	encrypted, err := seal(s.Key, priv)
	if err != nil {
		http.Error(w, "key encryption failed", 500)
		return
	}
	id, token := randomToken(), randomToken()
	expires := time.Now().UTC().Add(15 * time.Minute)
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), "INSERT INTO nodes(id,name,address,port,public_key,private_key) VALUES($1,$2,$3,$4,$5,$6)", id, in.Name, in.Address, in.Port, pub, encrypted)
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO enrollments(token_hash,node_id,expires_at) VALUES($1,$2,$3)", tokenHash(token), id, expires)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), "INSERT INTO team_nodes(team_id,node_id) VALUES($1,$2)", actor(r).Team, id)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "could not create enrollment", 500)
		return
	}
	command := setupCommand(s.PublicURL, token)
	respond(w, 201, map[string]any{"id": id, "state": "pending", "expires_at": expires, "setup_command": command})
}
func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), "SELECT id,name,address,port,state,capabilities,mode,last_checked_at,(SELECT max(expires_at) FROM enrollments e WHERE e.node_id=nodes.id AND e.consumed_at IS NULL) FROM nodes WHERE ($1 OR EXISTS(SELECT 1 FROM team_nodes tn WHERE tn.node_id=nodes.id AND tn.team_id=$2)) ORDER BY created_at", actor(r).Admin, actor(r).Team)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	type node struct {
		EnrollmentExpires *time.Time      `json:"enrollment_expires_at"`
		ID                string          `json:"id"`
		Name              string          `json:"name"`
		Address           string          `json:"address"`
		Port              int             `json:"port"`
		State             string          `json:"state"`
		Capabilities      json.RawMessage `json:"capabilities"`
		Mode              string          `json:"mode"`
		LastChecked       *time.Time      `json:"last_checked_at"`
	}
	out := []node{}
	for rows.Next() {
		var n node
		if rows.Scan(&n.ID, &n.Name, &n.Address, &n.Port, &n.State, &n.Capabilities, &n.Mode, &n.LastChecked, &n.EnrollmentExpires) != nil {
			http.Error(w, "could not read nodes", 500)
			return
		}
		if !actor(r).Admin {
			n.Address = ""
			n.Port = 0
			n.EnrollmentExpires = nil
			var capabilities map[string]json.RawMessage
			if len(n.Capabilities) > 0 && string(n.Capabilities) != "null" {
				if err := json.Unmarshal(n.Capabilities, &capabilities); err != nil {
					http.Error(w, "invalid node capabilities", 500)
					return
				}
				delete(capabilities, "metrics")
				delete(capabilities, "shared_host")
				n.Capabilities, _ = json.Marshal(capabilities)
			}
		}
		out = append(out, n)
	}
	if rows.Err() != nil {
		http.Error(w, "could not read nodes", 500)
		return
	}
	respond(w, 200, out)
}
func (s *Server) enrollment(r *http.Request) (string, string, error) {
	var id, pub string
	err := s.DB.QueryRow(r.Context(), "SELECT n.id,n.public_key FROM enrollments e JOIN nodes n ON n.id=e.node_id WHERE e.token_hash=$1 AND e.consumed_at IS NULL AND e.expires_at>now() AND NOT EXISTS(SELECT 1 FROM node_identity_rotations i WHERE i.node_id=n.id AND i.phase NOT IN ('succeeded','cancelled'))", tokenHash(bearer(r))).Scan(&id, &pub)
	return id, pub, err
}
func (s *Server) install(w http.ResponseWriter, r *http.Request) {
	id, pub, err := s.enrollment(r)
	if err != nil {
		http.Error(w, "invalid or expired enrollment", 401)
		return
	}
	sums := map[string]string{}
	for _, arch := range []string{"amd64", "arm64"} {
		b, err := os.ReadFile(filepath.Join(s.BinaryDir, "cove-worker-linux-"+arch))
		if err != nil {
			http.Error(w, "worker release unavailable", 503)
			return
		}
		hash := sha256.Sum256(b)
		sums[arch] = hex.EncodeToString(hash[:])
	}
	t, err := template.ParseFS(assets, "install.sh.tmpl", "worker-update*.sh.tmpl")
	if err != nil {
		http.Error(w, "installer unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript")
	_ = t.Execute(w, map[string]any{"ID": id, "Token": bearer(r), "PublicKey": pub, "URL": s.PublicURL, "AMD64": sums["amd64"], "ARM64": sums["arm64"]})
}
func (s *Server) binary(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.enrollment(r); err != nil {
		http.Error(w, "invalid or expired enrollment", 401)
		return
	}
	arch := r.PathValue("arch")
	if arch != "amd64" && arch != "arm64" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.BinaryDir, "cove-worker-linux-"+arch))
}
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		HostKey         string `json:"host_key"`
		EnrollmentError string `json:"enrollment_error"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.EnrollmentError != "" {
		if in.EnrollmentError != "trusted_verifier_missing" || in.HostKey != "" {
			http.Error(w, "invalid enrollment report", 400)
			return
		}
		s.provisionVerifierMissing(w, r)
		return
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(in.HostKey))
	if err != nil || len(options) != 0 || len(strings.TrimSpace(string(rest))) != 0 {
		http.Error(w, "invalid host public key", 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var id string
	err = tx.QueryRow(r.Context(), "SELECT node_id FROM enrollments WHERE token_hash=$1 AND consumed_at IS NULL AND expires_at>now()", tokenHash(bearer(r))).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "invalid or expired enrollment", 401)
		return
	}
	if err != nil {
		http.Error(w, "registration failed", 500)
		return
	}
	canonical := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	var existing *string
	if err = tx.QueryRow(r.Context(), "SELECT host_key FROM nodes WHERE id=$1 FOR UPDATE", id).Scan(&existing); err != nil {
		http.Error(w, "registration failed", 500)
		return
	}
	if existing != nil && *existing != canonical {
		http.Error(w, "registered host key does not match; enrollment cannot replace a pinned host identity", 409)
		return
	}
	tag, err := tx.Exec(r.Context(), "UPDATE enrollments SET consumed_at=now() WHERE token_hash=$1 AND consumed_at IS NULL AND expires_at>now()", tokenHash(bearer(r)))
	if err != nil {
		http.Error(w, "registration failed", 500)
		return
	}
	if tag.RowsAffected() != 1 {
		http.Error(w, "invalid or expired enrollment", 401)
		return
	}
	_, err = tx.Exec(r.Context(), "UPDATE nodes SET host_key=$1,state='enrolled' WHERE id=$2", canonical, id)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		http.Error(w, "registration failed", 500)
		return
	}
	r.SetPathValue("id", id)
	s.verifyNode(w, r)
}
func (s *Server) verifyNode(w http.ResponseWriter, r *http.Request) {
	var address string
	var port int
	var host *string
	var encrypted []byte
	var mode string
	err := s.DB.QueryRow(r.Context(), "SELECT address,port,host_key,private_key,mode FROM nodes WHERE id=$1", r.PathValue("id")).Scan(&address, &port, &host, &encrypted, &mode)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if host == nil && mode != "local" {
		http.Error(w, "node has not enrolled", 409)
		return
	}
	response, probeErr := s.nodeRPC(r.Context(), r.PathValue("id"), worker.RPCRequest{Method: "probe"})
	if err := s.noteHostIdentity(r.Context(), r.PathValue("id"), probeErr); err != nil {
		http.Error(w, "could not save host identity observation", 503)
		return
	}
	var p worker.Probe
	if response.Probe != nil && response.Probe.Protocol == 1 {
		p = *response.Probe
	} else if probeErr == nil {
		probeErr = errors.New("invalid worker response")
	}
	state := "unreachable"
	if probeErr == nil {
		state = "ssh_verified"
		if p.Ready {
			state = "ready"
		}
	}
	data, err := json.Marshal(p)
	if err != nil {
		http.Error(w, "invalid probe response", 500)
		return
	}
	_, err = s.DB.Exec(r.Context(), "UPDATE nodes SET state=$1,capabilities=$2,last_checked_at=now() WHERE id=$3 AND address=$4 AND port=$5", state, data, r.PathValue("id"), address, port)
	if err != nil {
		http.Error(w, "could not save verification", 500)
		return
	}
	respond(w, 200, map[string]any{"state": state, "capabilities": p})
}
