package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"cove/internal/worker"
)

type publicRoute struct {
	Method, Path, Scope, Summary string
	Input, Output                any
	Status                       int
	Handler                      http.HandlerFunc
}
type apiNameInput struct {
	Name string `json:"name"`
}
type apiOperationInput struct {
	Reason     string               `json:"reason,omitempty"`
	Action     string               `json:"action"`
	Release    string               `json:"release,omitempty"`
	BackupID   string               `json:"backup_id,omitempty"`
	PullLatest bool                 `json:"pull_latest,omitempty"`
	Build      *worker.BuildOptions `json:"build,omitempty"`
}
type apiID struct {
	ID string `json:"id"`
}
type apiOK struct {
	OK bool `json:"ok"`
}
type apiEnvironmentFile struct {
	Dotenv string `json:"dotenv"`
}
type apiVariableInput struct {
	Value string `json:"value"`
}
type apiVariableView struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
type apiTunnelInput struct {
	Tool      string `json:"tool,omitempty"`
	Service   string `json:"service"`
	Port      int    `json:"port"`
	PublicKey string `json:"public_key"`
}
type apiTunnelView struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
	Host      string    `json:"host"`
	SSHPort   int       `json:"ssh_port"`
	HostKey   string    `json:"host_key"`
}

func (s *Server) publicRoutes() []publicRoute {
	return []publicRoute{
		{"GET", "/managed-registries", "workloads:read", "Read team managed-registry metadata without credentials", nil, map[string]any{}, 200, s.managedRegistries},
		{"POST", "/managed-registries", "workloads:write", "Team administrators provision an authenticated registry through normal deployment admission", managedRegistryInput{}, map[string]string{}, 202, s.managedRegistries},
		{"PUT", "/managed-registries/{id}", "configuration:write", "Team administrators save retention or repair a failed certificate selection", managedRegistryChangeInput{}, apiOK{}, 200, s.managedRegistries},
		{"POST", "/managed-registries/{id}/retry", "workloads:operate", "Team administrators retry failed provisioning through normal deployment admission", nil, map[string]string{}, 202, s.managedRegistries},
		{"POST", "/managed-registries/{id}/gc", "workloads:operate", "Team administrators queue offline retention under approval, freeze and deployment windows", nil, map[string]string{}, 202, s.managedRegistries},
		{"DELETE", "/managed-registries/{id}", "workloads:write", "Team administrators remove a registry, retaining data unless explicitly requested and unreferenced", managedRegistryRemoveInput{}, map[string]string{}, 202, s.managedRegistries},
		{"DELETE", "/managed-registries/{id}/record", "workloads:write", "Team administrators forget only a removed registry whose data has been purged", nil, apiOK{}, 200, s.deleteManagedRegistryRecord},
		{"GET", "/workloads/{id}/image-scanning", "workloads:read", "Read team/node scanner opt-in metadata", nil, map[string]any{}, 200, s.imageScanning},
		{"PUT", "/workloads/{id}/image-scanning", "configuration:write", "Change team/node scan opt-in; team administrator required", imageScanningInput{}, map[string]any{}, 200, s.imageScanning},
		{"GET", "/workloads/{id}/image-scans", "workloads:read", "Read bounded vulnerability findings and unknown/stale coverage", nil, map[string]any{}, 200, s.workloadImageScans},
		{"GET", "/workloads/{id}/image-update-policy", "workloads:read", "Read image update policy and candidate metadata", nil, map[string]any{}, 200, s.imageUpdatePolicy},
		{"PUT", "/workloads/{id}/image-update-policy", "workloads:write", "Save image update ownership; enabling requires Operate and an operate-granting token scope", worker.ImageUpdatePolicy{}, map[string]any{}, 200, s.imageUpdatePolicy},
		{"GET", "/workloads/{id}/registry-webhook", "workloads:read", "Read registry webhook metadata without its token", nil, registryWebhookStatus{}, 200, s.registryWebhook},
		{"PUT", "/workloads/{id}/registry-webhook", "workloads:write", "Save registry webhook ownership; enabling also requires Operate and workloads:operate token scope", registryWebhookInput{}, registryWebhookStatus{}, 200, s.registryWebhook},
		{"POST", "/workloads/{id}/registry-webhook/rotate", "workloads:write", "Rotate webhook token once and claim ownership; also requires Operate and workloads:operate token scope", nil, registryWebhookStatus{}, 200, s.registryWebhook},
		{"DELETE", "/workloads/{id}/registry-webhook", "workloads:write", "Remove registry webhook and cancel unsubmitted owned deployments", nil, apiOK{}, 200, s.registryWebhook},
		{"GET", "/nodes/{id}/cloudflare-tunnel", "nodes:manage", "Inspect sealed tunnel publishing metadata; instance administrators only", nil, nodeTunnelStatus{}, 200, s.nodeCloudflareTunnel},
		{"PUT", "/nodes/{id}/cloudflare-tunnel", "nodes:manage", "Configure public exact-host tunnel publishing with an owned Cloudflare connection", nodeTunnelConfiguration{}, nodeTunnelStatus{}, 202, s.nodeCloudflareTunnel},
		{"DELETE", "/nodes/{id}/cloudflare-tunnel", "nodes:manage", "Disable the connector and clean up only proven owned provider resources", nil, nodeTunnelStatus{}, 202, s.nodeCloudflareTunnel},
		{"POST", "/nodes/{id}/cloudflare-tunnel/retry", "nodes:manage", "Reconcile saved tunnel ownership without repeating uncertain creates", nil, nodeTunnelStatus{}, 202, s.nodeCloudflareTunnel},
		{"GET", "/nodes/{id}/ingress-listeners", "nodes:manage", "Inspect shared node listener metadata; instance administrators only", nil, worker.IngressExposureStatus{}, 200, s.ingressExposures},
		{"POST", "/nodes/{id}/ingress-listeners/initialize", "nodes:manage", "Initialize or recover the shared node proxy; instance administrators only", nil, worker.IngressExposureStatus{}, 200, s.initializeIngressExposures},
		{"POST", "/nodes/{id}/ingress-listeners/plan", "nodes:manage", "Review shared listener changes without applying them", worker.IngressExposureSettings{}, worker.IngressExposurePlan{}, 200, s.planIngressExposures},
		{"PUT", "/nodes/{id}/ingress-listeners", "nodes:manage", "Apply a reviewed listener plan with explicit interruption acknowledgement", worker.IngressExposureRequest{}, worker.IngressExposureStatus{}, 200, s.changeIngressExposures},
		{"GET", "/workloads/{id}/route-listeners", "workloads:read", "Read managed route listener metadata", nil, map[string]any{}, 200, s.routeListeners},
		{"GET", "/workloads/{id}/certificate-authority", "workloads:read", "Read configured authority metadata and current terms", nil, map[string]string{}, 200, s.certificateAuthority},
		{"GET", "/workloads/{id}/pitr", "workloads:read", "Read PITR configuration metadata and verified recovery windows without keys", nil, map[string]any{}, 200, s.getPITR},
		{"PUT", "/workloads/{id}/pitr", "pitr:configure", "Configure PostgreSQL capture after explicit restart confirmation", pitrSettingsInput{}, apiID{}, 202, s.changePITR},
		{"DELETE", "/workloads/{id}/pitr", "pitr:forget", "Delete disabled PITR history after confirmation", pitrSettingsInput{}, apiID{}, 202, s.changePITR},
		{"POST", "/workloads/{id}/pitr/backup", "pitr:backup", "Queue a PITR base backup", nil, apiID{}, 202, s.changePITR},
		{"POST", "/workloads/{id}/pitr/restore-copy", "pitr:restore", "Recover a separate offline resource; source backup and secret-read plus destination create permissions apply; credentials returned once", pitrCopyInput{}, map[string]any{}, 202, s.restorePITR},

		{"GET", "/settings/directory/teams", "directory:manage", "List directory mapping targets; instance administrators only", nil, map[string]any{}, 200, s.directoryTeams},
		{"GET", "/settings/github-team-sync", "directory:manage", "Read sealed GitHub directory source metadata; instance administrators only", nil, []githubTeamSource{}, 200, s.githubTeamSources},
		{"POST", "/settings/github-team-sync", "directory:manage", "Create a GitHub group source; instance administrators only", githubTeamSourceInput{}, apiID{}, 200, s.githubTeamSources},
		{"PUT", "/settings/github-team-sync/{source}", "directory:manage", "Update a versioned GitHub group source; instance administrators only", githubTeamSourceInput{}, apiID{}, 200, s.githubTeamSources},
		{"DELETE", "/settings/github-team-sync/{source}", "directory:manage", "Remove a confirmed GitHub group source; instance administrators only", githubTeamDeleteInput{}, apiID{}, 200, s.githubTeamSources},
		{"POST", "/settings/github-team-sync/{source}/preview", "directory:manage", "Review directory membership differences; instance administrators only", githubTeamPreviewInput{}, githubDirectoryReport{}, 200, s.githubTeamPreview},
		{"POST", "/settings/github-team-sync/{source}/reconcile", "directory:manage", "Apply the confirmed current roster; instance administrators only", githubTeamPreviewInput{}, githubDirectoryReport{}, 200, s.githubTeamPreview},
		{"GET", "/settings/scim", "directory:manage", "Read provisioning metadata and prefixed base URL; instance administrators only", nil, map[string]any{}, 200, s.scimSettings},
		{"PUT", "/settings/scim", "directory:manage", "Configure separately authenticated provisioning; instance administrators only", scimSettingsInput{}, apiOK{}, 200, s.scimSettings},
		{"POST", "/settings/scim/token", "directory:manage", "Rotate the separate provisioning credential; returned once; instance administrators only", scimSettingsInput{}, map[string]any{}, 200, s.scimSettings},
		{"GET", "/settings/scim/activity", "directory:manage", "Read provisioning audit outcomes; instance administrators only", nil, map[string]any{}, 200, s.scimActivity},
		{"GET", "/settings/scim/groups", "directory:manage", "Read provisioning group mappings; instance administrators only", nil, map[string]any{}, 200, s.scimGroupMappings},
		{"PUT", "/settings/scim/groups/{group}/mappings", "directory:manage", "Confirm a current group role mapping; instance administrators only", scimMappingsInput{}, apiOK{}, 200, s.scimGroupMappings},
		{"GET", "/settings/scim/links", "directory:manage", "Read explicitly adopted provisioning links; instance administrators only", nil, []map[string]any{}, 200, s.scimAccountLink},
		{"POST", "/settings/scim/links", "directory:manage", "Explicitly adopt an existing account into provisioning; instance administrators only", scimLinkInput{}, apiOK{}, 200, s.scimAccountLink},
		{"DELETE", "/settings/scim/links/{user}", "directory:manage", "Detach provisioning ownership without enabling an account; instance administrators only", scimLinkInput{}, apiOK{}, 200, s.scimAccountLink},
		{"GET", "/settings/ldap/providers", "directory:manage", "Read directory provider metadata without bind credentials; instance administrators only", nil, []map[string]any{}, 200, s.ldapProviders},
		{"POST", "/settings/ldap/providers", "directory:manage", "Seal a verified LDAP provider and role mappings; instance administrators only", ldapProviderInput{}, map[string]any{}, 200, s.ldapProviders},
		{"PUT", "/settings/ldap/providers/{provider}", "directory:manage", "Update a versioned LDAP provider; instance administrators only", ldapProviderInput{}, apiOK{}, 200, s.ldapProviders},
		{"DELETE", "/settings/ldap/providers/{provider}", "directory:manage", "Remove a confirmed LDAP provider; instance administrators only", ldapProviderInput{}, apiOK{}, 200, s.ldapProviders},
		{"POST", "/settings/ldap/providers/{provider}/test", "directory:manage", "Test verified LDAP through the dedicated host CIDR policy; instance administrators only", nil, apiOK{}, 200, s.ldapTest},
		{"GET", "/workloads/{id}/certificates", "workloads:read", "List public certificate metadata without key material", nil, []routeCertificateView{}, 200, s.routeCertificates},
		{"POST", "/workloads/{id}/certificates", "certificates:write", "Seal an immutable resource certificate version", routeCertificateInput{}, routeCertificateView{}, 201, s.routeCertificates},
		{"DELETE", "/workloads/{id}/certificates/{certificate}", "certificates:write", "Delete an unused certificate version", nil, map[string]string{}, 200, s.routeCertificates},
		{"GET", "/workloads/{id}/certificate-zones", "workloads:read", "List the resource team's shared certificate zones", nil, []certificateZone{}, 200, s.certificateZones},
		{"GET", "/workloads/{id}/certificate-issuers", "workloads:read", "List certificate issuer metadata and reconciliation state", nil, []certificateIssuer{}, 200, s.certificateIssuers},
		{"POST", "/workloads/{id}/certificate-issuers", "certificates:write", "Checkpoint a sealed Origin CA request for an authorised zone", certificateIssuerInput{}, certificateIssuerAccepted{}, 202, s.certificateIssuers},
		{"PUT", "/workloads/{id}/certificate-issuers/{issuer}", "certificates:write", "Change renewal or retry a failed issuer", certificateIssuerUpdate{}, apiOK{}, 200, s.updateCertificateIssuer},
		{"DELETE", "/workloads/{id}/certificate-issuers/{issuer}", "certificates:write", "Remove issuer automation without deleting its certificate", nil, apiOK{}, 200, s.updateCertificateIssuer},
		{"GET", "/database-engines", "workloads:read", "List managed database engines and pinned templates", nil, map[string]any{}, 200, s.databaseEngineOptions},
		{"GET", "/workloads/{id}/database", "workloads:read", "Read database metadata and authorised dependent redeploy flags", nil, map[string]any{}, 200, s.databaseDetails},
		{"GET", "/workloads/{id}/database/credentials", "databases:manage", "Read audited database credentials; edit and secret permissions required", nil, map[string]string{}, 200, s.databaseDetails},
		{"GET", "/workloads/{id}/database/maintenance", "workloads:read", "Read retained database maintenance and cut-over capabilities", nil, []map[string]any{}, 200, s.databaseMaintenance},
		{"POST", "/workloads/{id}/database/maintenance", "databases:manage", "Explicit database maintenance or separate upgrade copy; normal deployment approval, freeze and window admission applies", databaseMaintenanceInput{}, map[string]string{}, 202, s.databaseMaintenance},
		{"POST", "/workloads/{id}/database/maintenance/{task}/retry", "databases:manage", "Retry the same immutable retained maintenance after an ambiguous result", nil, map[string]string{}, 202, s.databaseMaintenance},
		{"POST", "/workloads/{id}/database/maintenance/{task}/discard", "databases:manage", "Discard only a failed upgrade copy after confirmation", databaseDiscardInput{}, apiOK{}, 200, s.discardDatabaseCopy},
		{"POST", "/workloads/{id}/database/maintenance/{task}/cutover", "databases:manage", "Team administrators explicitly rewire bindings after stopped-writers acknowledgement; source is retained and consumers are not deployed", databaseCutoverInput{}, map[string]any{}, 200, s.cutoverDatabaseCopy},
		{"POST", "/workloads/{id}/database/preflight", "databases:manage", "Inspect engine compatibility before a major-version copy", databasePreflightInput{}, map[string]any{}, 200, s.databasePreflight},
		{"POST", "/workloads/{id}/database/pooler", "databases:manage", "Save and queue a normal admitted PostgreSQL pooler configuration deployment", databaseSettingsInput{}, map[string]string{}, 202, s.databaseSettings},
		{"POST", "/workloads/{id}/database/public-access", "databases:manage", "Save sealed native-TLS configuration and queue a normal admitted deployment", databaseSettingsInput{}, map[string]string{}, 202, s.databaseSettings},
		{"GET", "/workloads/{id}/object-buckets", "workloads:read", "List scoped Garage bucket metadata without credentials", nil, []map[string]any{}, 200, s.objectBuckets},
		{"POST", "/workloads/{id}/object-buckets", "databases:manage", "Team administrators provision one scoped Garage bucket and optional existing-model backup destination", objectBucketInput{}, map[string]any{}, 201, s.objectBuckets},
		{"POST", "/workloads/{id}/object-buckets/{bucket}/retry", "databases:manage", "Retry retained Garage provisioning credentials", nil, map[string]any{}, 201, s.objectBuckets},
		{"DELETE", "/workloads/{id}/object-buckets/{bucket}", "databases:manage", "Revoke a scoped key and bindings while retaining bucket data", struct {
			Confirm bool `json:"confirm"`
		}{}, apiOK{}, 200, s.objectBuckets},
		{"GET", "/workloads/{id}/variable-bindings", "workloads:read", "Read variable-binding references", nil, []resourceVariableBinding{}, 200, s.resourceVariables},
		{"PUT", "/workloads/{id}/variable-bindings", "workloads:write", "Bind a variable; secret values separately require current source secret-read permission", resourceVariableBinding{}, apiOK{}, 200, s.resourceVariables},
		{"DELETE", "/workloads/{id}/variable-bindings", "workloads:write", "Remove a variable binding", resourceVariableBinding{}, apiOK{}, 200, s.resourceVariables},

		{"GET", "/session", "", "Read the token creator and selected team", nil, identity{}, 200, s.sessionInfo},
		{"GET", "/nodes", "workloads:read", "List accessible worker metadata", nil, []map[string]any{}, 200, s.listNodes},
		{"GET", "/nodes/{id}/adoptions", "migration:adopt", "List node adoption reviews and recovery state; instance administrators only", nil, []map[string]any{}, 200, s.composeAdoptions},
		{"POST", "/nodes/{id}/adoptions/discover", "migration:adopt", "Inspect unmanaged Compose project metadata without secrets", nil, map[string]any{}, 200, s.composeAdoptions},
		{"POST", "/nodes/{id}/adoptions", "migration:adopt", "Map source projects into a reviewed canonical configuration without changing containers", composeAdoptionInput{}, map[string]any{}, 201, s.composeAdoptions},
		{"GET", "/nodes/{id}/adoptions/{adoption}", "migration:adopt", "Read a redacted adoption review", nil, map[string]any{}, 200, s.composeAdoptions},
		{"DELETE", "/nodes/{id}/adoptions/{adoption}", "migration:adopt", "Delete an unstarted adoption review", nil, apiOK{}, 200, s.composeAdoptions},
		{"POST", "/nodes/{id}/adoptions/{adoption}/refresh", "migration:adopt", "Reconcile worker ownership and refresh pending source reviews without changing source containers; review and confirm adoption again afterwards", nil, map[string]any{}, 200, s.composeAdoptions},
		{"POST", "/nodes/{id}/adoptions/{adoption}/adopt", "migration:adopt", "Explicitly adopt reviewed containers without restarting them; retry the same review after interruption", struct {
			Acknowledge   bool `json:"acknowledge"`
			ManagerPaused bool `json:"manager_paused"`
		}{}, map[string]any{}, 200, s.composeAdoptions},
		{"GET", "/projects", "projects:read", "List accessible projects and environments", nil, []project{}, 200, s.listProjects},
		{"POST", "/projects", "projects:write", "Create a project", apiNameInput{}, apiID{}, 201, s.createProject},
		{"GET", "/projects/{id}", "projects:read", "Read a project", nil, map[string]string{}, 200, s.publicGroup},
		{"PUT", "/projects/{id}", "projects:write", "Rename a project", apiNameInput{}, apiID{}, 200, s.publicGroup},
		{"DELETE", "/projects/{id}", "projects:write", "Delete an empty project", nil, apiOK{}, 200, s.deleteProject},
		{"POST", "/projects/{id}/environments", "projects:write", "Create an environment", apiNameInput{}, apiID{}, 201, s.createEnvironment},
		{"GET", "/environments/{id}", "projects:read", "Read an environment", nil, map[string]string{}, 200, s.publicGroup},
		{"PUT", "/environments/{id}", "projects:write", "Rename an environment", apiNameInput{}, apiID{}, 200, s.publicGroup},
		{"DELETE", "/environments/{id}", "projects:write", "Delete an empty environment", nil, apiOK{}, 200, s.deleteEnvironment},
		{"GET", "/projects/{id}/configuration", "projects:read", "Read the canonical project declaration with secret values omitted", nil, struct {
			Version     string               `json:"version"`
			ResourceIDs map[string]string    `json:"resource_ids"`
			Manifest    projectConfiguration `json:"manifest"`
		}{}, 200, s.projectConfigurationHandler},
		{"POST", "/projects/{id}/configuration/export", "secrets:read", "Export an audited private ZIP of cove.yaml, plain Compose files and resolved .env files; Git supporting files require the pinned repository working tree", struct {
			EnvironmentID string `json:"environment_id,omitempty"`
		}{}, struct{}{}, 200, s.exportProjectConfiguration},
		{"GET", "/projects/{id}/configuration/compare", "projects:read", "Compare two persistent environments by resource name; secret values are omitted", nil, map[string]any{}, 200, s.compareEnvironments},
		{"GET", "/projects/{id}/configuration/drafts", "configuration:write", "List redacted saved diffs", nil, []map[string]any{}, 200, s.configurationDrafts},
		{"POST", "/projects/{id}/configuration/drafts", "configuration:write", "Validate cove.yaml and save an encrypted draft without applying or deploying", struct {
			Content string `json:"content"`
		}{}, map[string]any{}, 201, s.configurationDrafts},
		{"DELETE", "/projects/{id}/configuration/drafts/{draft}", "configuration:write", "Delete a saved configuration draft", nil, apiOK{}, 200, s.configurationDrafts},
		{"POST", "/projects/{id}/configuration/drafts/{draft}/apply", "configuration:write", "Apply a reviewed declaration if its baseline is current; route changes may queue normal route operations, deployments remain explicit", nil, map[string]any{}, 200, s.applyConfigurationDraft},
		{"GET", "/projects/{id}/imports", "configuration:write", "List unexpired import session identifiers", nil, []map[string]any{}, 200, s.migrationImports},
		{"POST", "/projects/{id}/imports/coolify", "configuration:write", "Discover Coolify Compose API exports without contacting the source", struct {
			Items []coolifyImportItem `json:"items"`
		}{}, map[string]any{}, 201, s.migrationImports},
		{"GET", "/projects/{id}/imports/{import}", "configuration:write", "Read import candidate metadata without source secrets", nil, map[string]any{}, 200, s.migrationImports},
		{"DELETE", "/projects/{id}/imports/{import}", "configuration:write", "Delete an import session without changing drafts or source workloads", nil, apiOK{}, 200, s.migrationImports},
		{"POST", "/projects/{id}/imports/{import}/dry-run", "configuration:write", "Map selected imports and stage an encrypted canonical configuration draft; no containers are changed", importDryRunInput{}, map[string]any{}, 201, s.migrationImports},
		{"GET", "/projects/{id}/configuration/gitops", "projects:read", "Read GitOps status and declaration drift without values", nil, map[string]any{}, 200, s.projectGitOps},
		{"PUT", "/projects/{id}/configuration/gitops", "configuration:write", "Connect cove.yaml from an existing project Git resource; automatic configuration apply is explicitly opt-in", gitOpsInput{}, map[string]any{}, 200, s.projectGitOps},
		{"DELETE", "/projects/{id}/configuration/gitops", "configuration:write", "Disconnect a GitOps source without changing workloads", nil, apiOK{}, 200, s.projectGitOps},
		{"POST", "/projects/{id}/configuration/gitops/sync", "configuration:write", "Fetch cove.yaml and stage a pending diff, applying only if automatic apply is enabled", nil, map[string]any{}, 200, s.syncProjectGitOps},
		{"GET", "/workloads", "workloads:read", "List accessible resources", nil, []map[string]any{}, 200, s.listWorkloads},
		{"POST", "/workloads", "workloads:write", "Create a resource without deploying", workloadInput{}, apiID{}, 201, s.createWorkload},
		{"GET", "/workloads/{id}", "workloads:read", "Read resource configuration with secret values omitted", nil, workloadView{}, 200, s.getWorkload},
		{"PUT", "/workloads/{id}", "workloads:write", "Update saved configuration without deploying", workloadInput{}, apiID{}, 200, s.updateWorkload},
		{"POST", "/workloads/{id}/move/preview", "configuration:write", "Review a resource placement or ownership transfer without changing containers", resourceMoveInput{}, resourceMoveReview{}, 200, s.moveResource},
		{"POST", "/workloads/{id}/move", "configuration:write", "Apply a reviewed resource transfer; cross-team transfers require administration on both teams", resourceMoveInput{}, map[string]any{}, 200, s.moveResource},
		{"DELETE", "/workloads/{id}", "workloads:write", "Delete a stopped resource", nil, apiOK{}, 200, s.deleteWorkload},
		{"PUT", "/workloads/{id}/routes", "workloads:write", "Replace public routes using normal ingress reconciliation", struct {
			Routes []worker.Route `json:"routes"`
		}{}, map[string]any{}, 200, s.updateRoutes},
		{"POST", "/workloads/{id}/operations", "workloads:operate", "Queue an operation, including pull_latest deploy, rollout_promote/abort, image_check, image_scan and build; restore/drill additionally require workloads:restore, build requires Edit configuration and Read secrets", apiOperationInput{}, apiID{}, 202, s.queueOperation},
		{"GET", "/workloads/{id}/autoscale-policy", "workloads:read", "Read CPU replica autoscaling policy and state", nil, map[string]any{}, 200, s.autoscalePolicy},
		{"PUT", "/workloads/{id}/autoscale-policy", "workloads:write", "Set CPU replica autoscaling policy; queued scaling deploys use normal admission", worker.AutoscalePolicy{}, map[string]any{}, 200, s.autoscalePolicy},
		{"GET", "/workloads/{id}/idle-policy", "workloads:read", "Read idle sleep policy", nil, worker.IdlePolicy{}, 200, s.idlePolicy},
		{"PUT", "/workloads/{id}/idle-policy", "workloads:write", "Set idle sleep policy without deploying", worker.IdlePolicy{}, map[string]any{}, 202, s.idlePolicy},
		{"GET", "/workloads/{id}/restart-policy", "workloads:read", "Read unhealthy-container restart policy", nil, map[string]any{}, 200, s.unhealthyRestartPolicy},
		{"PUT", "/workloads/{id}/restart-policy", "workloads:write", "Set unhealthy-container restart policy", struct {
			Enabled bool `json:"enabled"`
		}{}, map[string]any{}, 200, s.unhealthyRestartPolicy},
		{"GET", "/environments/{id}/suspension", "projects:read", "Read environment pause and resume state", nil, map[string]any{}, 200, s.environmentSuspension},
		{"PUT", "/environments/{id}/suspension", "workloads:operate", "Pause or resume retained environment containers; recovery resumes do not deploy new releases", struct {
			Paused bool `json:"paused"`
		}{}, map[string]any{}, 202, s.environmentSuspension},
		{"GET", "/workloads/{id}/configuration-diff", "workloads:read", "Compare saved configuration with the last successful deployment declaration; secret values are omitted", nil, map[string]any{}, 200, s.workloadConfigurationDiff},
		{"GET", "/workloads/{id}/drift", "workloads:read", "Inspect runtime configuration changes since the last successful Cove operation without returning values", nil, worker.DriftReport{}, 200, s.workloadDrift},
		{"GET", "/workloads/{id}/compose-bundle", "workloads:read", "Read ordered Compose selection metadata without file contents", nil, map[string]any{}, 200, s.composeBundle},
		{"PUT", "/workloads/{id}/compose-bundle", "workloads:write", "Save ordered Compose overrides and profiles without deploying; null bundle clears the selection", struct {
			Bundle *worker.ComposeBundle `json:"bundle"`
		}{}, map[string]any{}, 200, s.composeBundle},
		{"POST", "/workloads/{id}/compose-bundle/export", "secrets:read", "Explicit audited read of private Compose override and ancillary-file contents", nil, struct {
			Bundle *worker.ComposeBundle `json:"bundle"`
		}{}, 200, s.composeBundle},
		{"GET", "/workloads/{id}/runtime", "workloads:read", "Read current container metadata", nil, struct {
			Containers []worker.Container `json:"containers"`
		}{}, 200, s.workloadRuntime},
		{"GET", "/workloads/{id}/releases", "workloads:read", "Read retained releases", nil, struct {
			Releases []worker.Release `json:"releases"`
		}{}, 200, s.workloadReleases},
		{"GET", "/workloads/{id}/logs", "logs:read", "Read a bounded container log snapshot; poll to follow", nil, struct {
			Logs string `json:"logs"`
		}{}, 200, s.workloadLogs},
		{"GET", "/workloads/{id}/metrics", "workloads:read", "Read bounded retained resource metrics; optional range query: hour, day, week, month or year", nil, map[string]any{}, 200, s.metricHistory},
		{"GET", "/workloads/{id}/log-history", "logs:read", "Read bounded retained redacted logs with optional query filters", nil, map[string]any{}, 200, s.logHistory},
		{"GET", "/workloads/{id}/logs/live", "logs:read", "Read a bounded redacted live-log batch; poll to follow", nil, map[string]any{}, 200, s.workloadLiveLogs},
		{"GET", "/workloads/{id}/traffic", "workloads:read", "Read bounded resource route traffic aggregates", nil, map[string]any{}, 200, s.workloadTraffic},
		{"GET", "/workloads/{id}/uptime", "workloads:read", "Read configured route check observations", nil, []uptimeCheck{}, 200, s.uptimeChecks},
		{"GET", "/workloads/{id}/sizing", "workloads:read", "Read observed sizing advice without changing limits", nil, map[string]any{}, 200, s.workloadSizing},
		{"GET", "/projects/{id}/costs", "projects:read", "Read allocation-based cost estimates for accessible resources", nil, map[string]any{}, 200, s.projectCosts},
		{"GET", "/operations", "workloads:read", "List up to 100 accessible operations; optional workload_id query filter", nil, []map[string]any{}, 200, s.listOperations},
		{"GET", "/operations/{id}", "workloads:read", "Read an operation", nil, operationView{}, 200, s.getOperation},
		{"GET", "/operations/{id}/logs", "logs:read", "Read bounded operation logs", nil, struct {
			Logs string `json:"logs"`
		}{}, 200, s.operationLogs},
		{"POST", "/operations/{id}/cancel", "workloads:operate", "Request operation cancellation", nil, map[string]string{}, 202, s.cancelOperation},
		{"POST", "/workloads/{id}/variables/import", "secrets:write", "Merge saved dotenv variables", apiEnvironmentFile{}, struct {
			Keys []string `json:"keys"`
		}{}, 200, s.environmentFile},
		{"POST", "/workloads/{id}/variables/resolve", "secrets:read", "Export resolved saved variables and known built-in metadata; audited before disclosure", nil, struct {
			Environment map[string]string `json:"environment"`
		}{}, 200, s.resolvedEnvironment},
		{"POST", "/workloads/{id}/variables/export", "secrets:read", "Export saved raw variables; audited before disclosure", nil, apiEnvironmentFile{}, 200, s.environmentFile},
		{"GET", "/workloads/{id}/variables/{variable}", "secrets:read", "Read existence and opaque revision, never a variable value", nil, apiVariableView{}, 200, s.publicVariable},
		{"PUT", "/workloads/{id}/variables/{variable}", "secrets:write", "Set one saved variable", apiVariableInput{}, apiVariableView{}, 200, s.publicVariable},
		{"DELETE", "/workloads/{id}/variables/{variable}", "secrets:write", "Remove one saved variable", nil, apiOK{}, 200, s.publicVariable},
		{"POST", "/workloads/{id}/exec", "exec:run", "Run a command with a 1-30 second wait and 64 KiB combined output; timeout does not confirm process termination", worker.ExecRequest{}, worker.ExecResult{}, 200, s.workloadExec},
		{"POST", "/workloads/{id}/database-access", "tunnels:write", "Open a 15 minute pinned SSH service tunnel lease", apiTunnelInput{}, apiTunnelView{}, 201, s.databaseAccess},
		{"DELETE", "/workloads/{id}/database-access", "tunnels:write", "Revoke a service tunnel lease", apiID{}, apiOK{}, 200, s.databaseAccess},
	}
}

func (s *Server) registerPublicAPI(mux *http.ServeMux) {
	for _, route := range s.publicRoutes() {
		mux.Handle(route.Method+" /api/v1"+route.Path, s.publicHandler(route))
	}
	mux.HandleFunc("GET /api/v1/openapi.json", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, s.openAPIDocument()) })
}
func (s *Server) publicHandler(route publicRoute) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.Clone(context.WithValue(r.Context(), apiRouteKey{}, route))
		r.URL.Path = strings.Replace(r.URL.Path, "/api/v1/", "/api/", 1)
		r.Pattern = route.Method + " /api" + route.Path
		s.admin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(route.Path, "/operations") {
				fields, ok := bodyFields(w, r)
				if !ok {
					return
				}
				if field(fields, "action") == "restore" || field(fields, "action") == "drill" {
					if token, ok := r.Context().Value(apiCredentialKey{}).(apiCredential); ok && !containsScope(token.Scopes, "workloads:restore") {
						http.Error(w, "token requires workloads:restore", 403)
						return
					}
				}
			}
			route.Handler(w, r)
		})).ServeHTTP(w, r)
	})
}

func containsScope(scopes []string, want string) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}

func apiSchema(t reflect.Type) map[string]any {
	if t == reflect.TypeFor[json.RawMessage]() {
		return map[string]any{}
	}
	if t.Kind() == reflect.Pointer {
		return map[string]any{"anyOf": []any{apiSchema(t.Elem()), map[string]any{"type": "null"}}}
	}
	if t == reflect.TypeFor[time.Time]() {
		return map[string]any{"type": "string", "format": "date-time"}
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": []string{"array", "null"}, "items": apiSchema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": []string{"object", "null"}, "additionalProperties": apiSchema(t.Elem())}
	case reflect.Struct:
		props := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			tag := strings.Split(field.Tag.Get("json"), ",")[0]
			if field.Anonymous && field.Type.Kind() == reflect.Struct && tag == "" {
				for name, schema := range apiSchema(field.Type)["properties"].(map[string]any) {
					props[name] = schema
				}
				continue
			}
			if !field.IsExported() || tag == "-" {
				continue
			}
			if tag == "" {
				tag = field.Name
			}
			props[tag] = apiSchema(field.Type)
			if tag == "private_key" || tag == "private_key_pem" || tag == "bind_password" {
				props[tag].(map[string]any)["writeOnly"] = true
			}
		}
		return map[string]any{"type": "object", "properties": props}
	default:
		return map[string]any{}
	}
}
func (s *Server) openAPIDocument() map[string]any {
	paths := map[string]any{}
	for _, route := range s.publicRoutes() {
		entry, ok := paths[route.Path].(map[string]any)
		if !ok {
			entry = map[string]any{}
			paths[route.Path] = entry
		}
		responses := map[string]any{fmt.Sprint(route.Status): map[string]any{"description": "Success", "content": map[string]any{"application/json": map[string]any{"schema": apiSchema(reflect.TypeOf(route.Output))}}}}
		if route.Path == "/projects/{id}/configuration/export" {
			responses["200"] = map[string]any{"description": "Private ZIP archive", "content": map[string]any{"application/zip": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}}}}
		}
		if route.Method == "PUT" && route.Path == "/workloads/{id}/routes" {
			responses["202"] = responses["200"]
		}
		for _, code := range []int{400, 401, 403, 404, 409, 413, 429, 500, 502, 503} {
			responses[fmt.Sprint(code)] = map[string]any{"description": http.StatusText(code), "content": map[string]any{"text/plain": map[string]any{"schema": map[string]any{"type": "string"}}}}
		}
		operation := map[string]any{"summary": route.Summary, "operationId": strings.ToLower(route.Method) + strings.NewReplacer("/", "_", "{", "", "}", "").Replace(route.Path), "x-cove-scope": route.Scope, "security": []any{map[string]any{"bearer": []any{}}}, "responses": responses}
		params := []any{map[string]any{"name": "X-Cove-Team", "in": "header", "required": false, "description": "Selected team for a user token; team tokens are fixed to their team.", "schema": map[string]any{"type": "string"}}}
		for _, part := range strings.Split(route.Path, "/") {
			if strings.HasPrefix(part, "{") {
				params = append(params, map[string]any{"name": strings.Trim(part, "{}"), "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
			}
		}
		if route.Path == "/projects/{id}/configuration/compare" {
			for _, name := range []string{"from", "to"} {
				params = append(params, map[string]any{"name": name, "in": "query", "required": true, "schema": map[string]any{"type": "string"}})
			}
		}
		optionalQueries := map[string][]string{
			"/workloads/{id}/metrics": {"range"}, "/workloads/{id}/log-history": {"q", "container", "before", "before_id"}, "/workloads/{id}/logs/live": {"since"}, "/workloads/{id}/traffic": {"range"},
		}
		for _, name := range optionalQueries[route.Path] {
			params = append(params, map[string]any{"name": name, "in": "query", "required": false, "schema": map[string]any{"type": "string"}})
		}
		operation["parameters"] = params
		if route.Input != nil {
			operation["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": apiSchema(reflect.TypeOf(route.Input))}}}
		}
		entry[strings.ToLower(route.Method)] = operation
	}
	document := map[string]any{"openapi": "3.1.0", "info": map[string]string{"title": "Cove public API", "version": "1.0.0", "description": "Scopes limit tokens; every request also revalidates its creator's current grants. Errors are plain text. Deployment is explicit and asynchronous. Secret values require explicit scoped operations or audited exports."}, "servers": []any{map[string]string{"url": s.panelBasePath() + "/api/v1"}}, "paths": paths, "components": map[string]any{"securitySchemes": map[string]any{"bearer": map[string]string{"type": "http", "scheme": "bearer"}}}}
	s.documentSCIM(document)
	return document
}

func (s *Server) publicGroup(w http.ResponseWriter, r *http.Request) {
	environment := strings.Contains(r.URL.Path, "/environments/")
	kind, table := "project", "projects"
	if environment {
		kind, table = "environment", "environments"
	}
	level := 1
	if r.Method != "GET" {
		level = 3
	}
	if !s.requireAccess(w, r, kind, r.PathValue("id"), level) {
		return
	}
	if r.Method == "GET" {
		var name, parent string
		query := `SELECT name,'' FROM projects WHERE id=$1 AND team_id=$2`
		if environment {
			query = `SELECT name,project_id FROM environments WHERE id=$1 AND team_id=$2`
		}
		if s.DB.QueryRow(r.Context(), query, r.PathValue("id"), actor(r).Team).Scan(&name, &parent) != nil {
			http.Error(w, "group unavailable", 503)
			return
		}
		respond(w, 200, map[string]string{"id": r.PathValue("id"), "name": name, "project_id": parent})
		return
	}
	var in apiNameInput
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 120 {
		http.Error(w, "name must contain 1-120 characters", 400)
		return
	}
	if _, err := s.DB.Exec(r.Context(), `UPDATE `+table+` SET name=$2 WHERE id=$1 AND team_id=$3`, r.PathValue("id"), in.Name, actor(r).Team); err != nil {
		groupError(w, err)
		return
	}
	respond(w, 200, apiID{r.PathValue("id")})
}
