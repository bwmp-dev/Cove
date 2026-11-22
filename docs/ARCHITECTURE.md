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

Managed HTTP routes optionally carry a validated policy for path prefixes, request/response headers, source CIDR restrictions, hostname redirects, ACME selection, sticky sessions and additional upstream Compose services on the same worker. Policies travel in the existing encrypted workload configuration and durable route journal; no separate route database is introduced. Policy-derived proxy object names make stale middleware/upstream configurations fail live confirmation. Workers advertise `route_policies`; older workers cannot deploy policies. This extension does not create replicas or change proxy entrypoints.

Route reads hide basic-auth hashes. Omitted authentication in configuration or route updates preserves the existing hostname/path credentials; an explicit empty value removes them. Prefix changes with ambiguous previous credentials are rejected, keeping policy edits from silently exposing a protected route.

Runtime inspection exposes Docker health separately from lifecycle state and reports exit codes plus successful Compose init completion. Job schedules use a saved IANA timezone (UTC for existing schedules), with embedded timezone data in the control plane. These changes do not alter container restart policies or replace the guarded-deploy algorithm: guarded deploys test a stateless candidate, then perform ordinary Compose replacement with a possible brief interruption; they are not zero-downtime route handoffs.

Image lifecycle checks are durable, cancellable metadata operations on upgraded workers. Background checks use the existing workload lock and queue, with at most four background checks at once, and run hourly for deployed, non-stopped services outside managed previews. Workers compare registry manifests and platform configuration digests against deployed images, using the resource's team registry connection. Jobs are excluded so metadata checks cannot consume scheduled run slots. Explicit image refresh remains an ordinary approved/frozen deploy operation with forced pulls; existing releases remain available for rollback.

Separate build operations build Compose services on their resource's worker and publish verified immutable references through the selected registry connection. They reuse encrypted requests, shared variables, Git credentials, file secrets, bounded logs and per-workload locking. Unique operation/service tags prevent concurrent builds from exchanging image identities; each published manifest is checked against the built image before recording success. Builds do not replace containers, routes or the current release. Temporary tags and inputs have an ownership-checked cleanup journal; interrupted or partially published artifacts remain inspectable in the registry. Build nodes, a managed registry and non-Dockerfile build engines are outside this implementation.

## Completion requirement

The user explicitly requires a fully fleshed-out, production-ready product, not a proof of concept. The delivery order above describes implementation increments, not acceptable stopping points. Release acceptance must cover authenticated dashboard workflows, durable execution and recovery, secret handling, Git/build deployment, routing, backup/restore, installation/upgrades, and real Linux validation. Unverified production behavior must remain explicitly identified.
