# Cove

Cove is a custom, lightweight deployment control plane, not a wrapper around another hosting panel.

## Agreed design

- Exactly one main node hosts the dashboard, API, PostgreSQL configuration database, and durable job queue.
- Worker nodes run Docker/Compose workloads. There is no artificial node limit; capacity depends on control-plane resources.
- The main node initiates SSH connections to workers. Workers need no public management API. Private networks, VPNs, and eventually jump hosts can provide reachability.
- Each worker uses a small Go executable with structured requests and responses. Long-running operations must persist locally and survive SSH disconnection. Reconnection reconciles operation IDs before retrying.
- The main node can also host workloads using the same executable through a local transport, without SSH to itself.
- Add node generates an expiring one-time setup command. Bootstrap uses HTTPS, installs prerequisites, configures a dedicated SSH identity, registers the host key, and verifies connectivity from the main node.
- Enrollment and readiness are distinct states. Existing Docker installations, workloads, and unrelated SSH configuration must be preserved.
- Workloads have explicit node placement. Compose stacks stay on one node. Cross-node networking is explicit.
- Public workers handle ingress/TLS locally; application traffic need not pass through the main node.
- Applications continue running when the control plane is offline.
- Projects, environments, workloads, releases, operations, routes, storage, and backup policies form the domain model.
- Secrets are encrypted at rest and delivered only where needed. Per-node SSH credentials and pinned host keys are mandatory.
- Builds, backups, and other expensive operations run on workers. Logs and telemetry are bounded.

## Delivery order

1. Persistent node enrollment, generated setup command, worker installation, SSH verification, and capability probe.
2. Compose deployment with durable worker operations, per-workload locking, logs, and interruption recovery.
3. Dashboard, projects/environments, Git automation, local workloads, and routing.
4. Database and volume backups, tested restores, dedicated build nodes, and Coolify migration.

Initial workers target Linux with systemd and OpenSSH. Other platforms and jump hosts are later work. A node with Docker access is a privileged infrastructure identity even when its SSH command is restricted.

## Implemented release scope

Individual email/password and GitHub OAuth accounts, optional open registration, verified email changes, password resets, invitations, teams, shared-worker assignment, and project/environment permission grants are implemented. Existing resources and the administrator migrate into a default team. Instance administration is separate from team ownership; API authorization scopes configuration, operations, logs, backups, tokens and audit records. Shared workers do not isolate hostile tenants: Docker/Compose access can compromise other workloads on the host, so mutually untrusted teams need separate workers or VMs.

Account acceptance covers legacy database/session migration, invitation expiry/replay/revocation, account disablement, password/session revocation, scoped resource isolation, token permission revalidation, and GitHub state/cookie/PKCE, verified-email, collision and linking checks against a controlled provider. Linux race tests and PostgreSQL integration pass. Packaged Chromium verifies invitation signup, team switching, environment-scoped viewer access, denied resource access, account disabling and password rotation alongside the real worker resource lifecycle. Actual GitHub and SMTP credentials must be configured and tested on each deployment; local provider/mail fixtures do not establish external delivery or provider acceptance.

Implemented: persistent enrollment, restricted SSH RPC, pinned host identities, local socket transport, browser sessions and password rotation, workload-scoped expiring/revocable API tokens, encrypted workload configuration with per-key secret updates, durable operations and cancellation, Compose services and one-shot jobs, Git checkouts, retained-image release rollback, bounded/redacted logs, periodic node checks, mutation audit records, and self-hosted packaging. The React/TanStack frontend supports these management workflows. Worker upgrades drain active operations, retain queued work across restarts, and restore the previous executable if replacement readiness fails. Writable Git-release bind mounts are rejected in favor of named volumes or stable host paths.

Verified locally: PostgreSQL enrollment/session integration, password rotation/session revocation, scoped-token isolation/revocation, per-key secret preservation, and panel backup/restore; real Docker/SSH deployments, rollback, cancellation, restart/replay, stop operations, and successful/failing jobs. Packaged Chromium tests cover authentication, enrollment, responsive node lists, deployment, live container state, rollback, token creation/revocation, and password rotation. A dedicated Ubuntu 24.04 QEMU VM passed generated installation, pinned SSH enrollment, local-socket deployment, application survival during upgrade, reenrollment, replacement readiness, and restoration after an intentionally broken replacement. These are local test-TLS checks, not public certificate or private Git-provider acceptance.

Projects and environments, signed GitHub push webhooks with durable deduplication and commit pinning, managed Traefik application ingress, local and S3 encrypted storage backups, scheduled backups, verified restores, audit browsing, release retention, safe workload/node deletion, and enrollment expiry are implemented. The optional local-worker Compose service shares the panel's HTTPS ingress through a protected Unix socket. Authenticated mutations and rejected authorization checks are audited without request bodies or credentials. Ten recent releases are retained, plus releases referenced by the current deployment or backup records; pruning only removes Cove-owned release images and source directories.

Linux acceptance covers Ubuntu 24.04 amd64 in an isolated QEMU VM: generated installation, successful upgrade, failed replacement recovery, private HTTPS Git authentication and pinned builds, rollback with the Git server unavailable, packaged main-node execution, shared dashboard/application TLS, and application survival during control-plane outages. Pebble acceptance exercises actual ACME challenges, trusted certificates, automatic renewal, proxy restart and CA outage. PostgreSQL volume backup/restore is tested with local storage and an encrypted S3 repository over verified TLS. Restore checks recover database contents, remove files created after the snapshot, retain a safety snapshot, and enforce retention. ARM64 worker unit tests pass under emulation; ARM64 systemd installation and other distributions are not runtime-certified.

Packaged Chromium acceptance includes project/environment assignment, masked backup-policy editing, deletion refusal for running workloads, stopped-workload deletion and pending-node deletion. Responsive screenshots cover 390, 768 and 1440 pixel widths. Inventory integration covers 2,048 synthetic nodes and workloads with 40 list reads from 20 concurrent clients; this is not a fleet-capacity benchmark. Worker execution and dispatcher concurrency remain bounded rather than growing with inventory size.

Deployment acceptance still requires the target site's DNS, firewall, SSH reachability and publicly trusted certificate issuance. Isolated ACME tests cannot prove those external conditions. No existing Coolify applications were migrated or modified. Dedicated build nodes, cross-node scheduling, jump hosts and automatic Coolify import are outside this release scope.

Live acceptance on an authorized Ubuntu amd64 VPS pair covered dashboard and worker installation, pinned SSH enrollment, public TLS issuance, login/session protections, remote deployment, persistent-volume backup and exact restore, and application survival during worker and dashboard restarts. Private host identifiers and operational recovery records are maintained outside this repository.

Traefik-specific acceptance covers automatic ACME renewal, certificate persistence, dynamic removal/restoration of the last application route, shared-panel continuity and management API isolation. Public TLS and remote deployments passed again after migration. Other hosts and platforms require their own deployment acceptance.

Rollback restores saved Compose configuration and retained local images; it does not reverse data or database migrations. Backups stop the workload for consistency and reject storage shared with other running containers. Restore requires the current storage layout, creates a safety snapshot and leaves the workload stopped if recovery fails. Local backups do not protect against node loss; offsite backup passwords and the panel encryption key must be backed up independently. Cancellation stops the command and leaves already-applied container changes for inspection. Operation journals and audit history are retained for reconciliation and investigation; operators must monitor total disk/database growth. Per-operation output is bounded, but historical storage is not globally capped.

Environment previews copy one production environment per branch name across selected repositories: matching branches build, everything else runs the production state pinned at creation, data is copied once from read-only sources into preview-owned storage, connections are recreated between the copies, and protected routes are published after every resource is healthy. See DEVELOPMENT.md for the exact semantics and limits.

Private connections are explicit, directional TCP grants from one source service to one target service port, enforced on workers by a per-node link gateway with dedicated internal networks per source hostname and per target, and pinned mutual TLS between workers. They do not replace or restrict shared networks, published ports or public routes. See DEVELOPMENT.md for the operating model and limits.

## Completion requirement

The user explicitly requires a fully fleshed-out, production-ready product, not a proof of concept. The delivery order above describes implementation increments, not acceptable stopping points. Release acceptance must cover authenticated dashboard workflows, durable execution and recovery, secret handling, Git/build deployment, routing, backup/restore, installation/upgrades, and real Linux validation. Unverified production behavior must remain explicitly identified.

### Git provider boundary

GitHub clone credentials, repository and branch listing, webhook signature verification, pull-request decoding and commit-status publishing are accessed through the control plane's internal `gitProvider` interface (`internal/control/git_provider.go`). Installation ownership and team sharing checks remain at the handler/deployment boundary; durable webhook deduplication, commit pinning, superseding and preview reconciliation remain in their existing queues. Clone tokens continue to request only repository-scoped contents read access. Commit-status publishing requests a separate repository-scoped statuses write token; clone tokens never gain write permissions. Automatic GitHub feedback is opt-in per source, inherited by its PR previews. A separate retry queue publishes immutable-commit statuses, Deployment records and App-authored preview comments without failing deployment execution.


### Additional Git sources and CI triggers

Team-owned Git connections support GitLab, Gitea-compatible servers (including Forgejo and Codeberg), and Bitbucket Cloud. Credentials and webhook signing keys are sealed; provider repositories have connection-scoped local identities, including Bitbucket UUID mappings. API calls require trusted HTTPS, public DNS/IP destinations and no redirects. Signed pushes enter the existing encrypted, deduplicated deployment queue with immutable commit revisions and service-push superseding. PR/MR events use the existing isolated preview lifecycle; forks remain excluded. Optional commit statuses and provider-account-authored preview comments retry independently of deployment execution. GitLab signing requires GitLab 19's signing-token support; plain secret-token headers are not accepted.

SSH sources use a generated Ed25519 identity per workload and an operator-supplied host public-key pin. The control plane seals the identity and injects it only into encrypted operation payloads. The worker uses a temporary identity and known-hosts file, refuses ambient/password authentication and unknown host identities, then removes the credentials on success or failure. Clones need their own generated deploy key. HTTPS credential helpers are restricted to the source's protocol, host and port.

Automatic source triggers can follow the saved branch, a tag glob, or published releases resolved to tag commits. Manual deployment continues to use the saved branch. GitHub, GitLab and Gitea-compatible release events are supported; Bitbucket Cloud has no native release event and uses tags. Inbound CI/CMS hooks are separately scoped credentials with a 90-day lifetime, ten new deliveries per minute, stable delivery receipts and creator-grant revalidation at receipt and dispatch. Rotation/revocation removes unsubmitted hook requests. The reusable composite Action under `.github/actions/deploy` triggers a hook and polls its durable receipt without placing credentials in URLs or following redirects.

### Variables and external secret providers

Saved `.env` import merges named keys under the resource configuration lock and blocks edits during an active operation. Parsing is bounded, does not execute shell syntax or expand dollars, and preserves quoted/multiline values. Export is an explicit editor-only POST, audited before disclosure and sent with `Cache-Control: no-store`; it contains saved values and references rather than resolving external secrets.

Deployment variable resolution composes encrypted workload values, existing shared/resource bindings and team-scoped external secret sources. References have the form `{{secret.CONNECTION_ID:REFERENCE}}`. Provider credentials are sealed independently and never returned by connection reads or writes. Resolved values are bounded, treated as opaque, redacted by the worker, and sealed into the operation's prepared payload; retries reuse that snapshot. Missing, revoked, malformed or inaccessible references fail deployment with the variable name and a sanitized explanation. Preview plans require explicit decisions for external secret and shared references rather than silently copying production credentials.

The resolver supports Vault/OpenBao KV v2, Infisical v4, Doppler, 1Password Connect and optional vendor CLI sources for 1Password service accounts and Bitwarden Secrets Manager. HTTP sources use trusted HTTPS, public-address DNS checks, bounded responses and no redirects. CLI sources use credentials only in isolated child environments, private temporary configuration, bounded output, discarded diagnostic output and read-only commands. Service-account tokens are limited to the official 1Password cloud domains; Bitwarden supports the US/EU clouds. Vendor CLIs are operator-installed prerequisites, not bundled dependencies, and missing binaries are reported clearly.

Workers advertising `builtin_variables` add seven deployment defaults: `COVE_FQDN`, `COVE_URL`, `COVE_COMMIT_SHA`, `COVE_BRANCH`, `COVE_ENVIRONMENT`, `COVE_PROJECT` and `COVE_RELEASE`. The actual checked-out commit supplies the SHA. Defaults are available to Compose interpolation and inserted into missing service environment/build-argument entries, preserving explicit settings. Retained release state records which metadata values are public so ordinary names do not corrupt secret-redacted logs. Older workers keep their existing behavior and the API reports built-ins unavailable until upgrade and verification.

### Public automation interfaces

A versioned `/api/v1` catalogue registers real handlers and generates OpenAPI 3.1 request/response schemas; Swagger UI is bundled in the frontend. Personal/team token credentials are stored only as hashes with explicit scopes and expiry. The token permission catalogue is centralized for reconciliation with custom roles; every request also revalidates the creator's active account, current membership and grants. Legacy workload-scoped tokens remain isolated to their existing routes. Explicit saved/resolved variable disclosure is editor/scoped and audited before response; ordinary variable reads expose only existence and a keyed revision.

The same `cove` binary retains no-argument server startup and supplies opt-in client commands with private named contexts, deploy/wait, polling logs, bounded container exec, environment pull/push/local-run and pinned temporary service forwarding. One-off exec selects running Compose-owned containers and bounds response output/wait; a timed-out Docker exec may continue inside the container and is never automatically replayed. Runtime service labels support replica selection without trusting user-provided container IDs.

MCP is an administrator-enabled, off-by-default stateless HTTP surface implemented with the official SDK. Scoped-token-only resource/tool calls reuse the public API authorization boundary and existing operation queue. An independent Terraform/OpenTofu protocol6 provider manages projects, environments, workloads, variables and complete public route sets through the same API; it does not implicitly deploy or migrate application data. Native provider fixtures are isolated from real accounts and hosts.

Compose compatibility is checked against the native plugin with local file fixtures, including retained-release round trips. Worker transforms distinguish resolved Compose's escaped representation from raw preview configuration, preserving literal dollars through limits, secrets, networking, promotion and release snapshots. Snapshots remove profile conditions after service selection so rollback restores that selection. Primary-file null network names are normalized in an adjacent private copy. Restricted PR previews are validated before any Compose command, including built-in variable resolution.
