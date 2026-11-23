-- Snapshot: merge: integrate deploys certificates

CREATE TABLE IF NOT EXISTS nodes (
 id text PRIMARY KEY,
 name text NOT NULL,
 address text NOT NULL,
 port integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
 public_key text NOT NULL,
 private_key bytea NOT NULL,
 host_key text,
 state text NOT NULL DEFAULT 'pending',
 capabilities jsonb,
 created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS mode text NOT NULL DEFAULT 'ssh';
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS last_checked_at timestamptz;
CREATE UNIQUE INDEX IF NOT EXISTS one_local_node ON nodes(mode) WHERE mode='local';
CREATE TABLE IF NOT EXISTS enrollments (
 token_hash bytea PRIMARY KEY,
 node_id text NOT NULL REFERENCES nodes(id),
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz
);
CREATE INDEX IF NOT EXISTS pending_enrollments ON enrollments(node_id,expires_at) WHERE consumed_at IS NULL;
CREATE TABLE IF NOT EXISTS workloads (
 id text PRIMARY KEY,
 node_id text NOT NULL REFERENCES nodes(id),
 name text NOT NULL,
 configuration bytea NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS projects (id text PRIMARY KEY, name text NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS environments (
 id text PRIMARY KEY,
 project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 name text NOT NULL,
 UNIQUE(project_id,name)
);
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS environment_id text REFERENCES environments(id);
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'service';
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS backup_interval integer NOT NULL DEFAULT 0;
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS next_backup_at timestamptz;
CREATE INDEX IF NOT EXISTS due_backups ON workloads(next_backup_at) WHERE backup_interval>0;
CREATE TABLE IF NOT EXISTS operations (
 id text PRIMARY KEY,
 workload_id text NOT NULL REFERENCES workloads(id),
 action text NOT NULL,
 state text NOT NULL DEFAULT 'queued',
 payload bytea NOT NULL,
 result jsonb,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS one_active_operation_per_workload ON operations(workload_id) WHERE state IN ('queued','running');
ALTER TABLE operations ADD COLUMN IF NOT EXISTS cancel_requested boolean NOT NULL DEFAULT false;
CREATE TABLE IF NOT EXISTS administrators (
 id integer PRIMARY KEY CHECK (id=1),
 password_hash bytea NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
 token_hash bytea PRIMARY KEY,
 expires_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS login_attempts (
 address text PRIMARY KEY,
 attempts integer NOT NULL,
 window_start timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS control_settings (
 id integer PRIMARY KEY CHECK (id=1),
 encryption_check bytea NOT NULL
);
ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS update_channel text NOT NULL DEFAULT 'release' CHECK (update_channel IN ('beta','release'));
CREATE TABLE IF NOT EXISTS audit_events (
 id bigserial PRIMARY KEY,
 actor text NOT NULL,
 action text NOT NULL,
 subject text NOT NULL,
 status integer NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS api_tokens (
 id text PRIMARY KEY,
 name text NOT NULL,
 token_hash bytea UNIQUE NOT NULL,
 workload_id text NOT NULL REFERENCES workloads(id),
 permission text NOT NULL CHECK(permission IN ('read','operate')),
 expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS webhook_secret bytea;
CREATE TABLE IF NOT EXISTS webhook_deliveries (
 workload_id text NOT NULL REFERENCES workloads(id),
 delivery text NOT NULL,
 payload bytea NOT NULL,
 operation_id text,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(workload_id,delivery)
);
CREATE INDEX IF NOT EXISTS pending_webhooks ON webhook_deliveries(created_at) WHERE operation_id IS NULL;

CREATE TABLE IF NOT EXISTS users (
 id text PRIMARY KEY,
 email text UNIQUE,
 name text NOT NULL,
 password_hash bytea,
 email_verified boolean NOT NULL DEFAULT false,
 is_admin boolean NOT NULL DEFAULT false,
 disabled boolean NOT NULL DEFAULT false,
 github_id text UNIQUE,
 auth_version bigint NOT NULL DEFAULT 0,
 created_at timestamptz NOT NULL DEFAULT now(),
 CHECK(email IS NULL OR email=lower(email))
);
INSERT INTO users(id,name,password_hash,email_verified,is_admin)
VALUES('bootstrap','Administrator',(SELECT password_hash FROM administrators WHERE id=1),true,true)
ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS teams (id text PRIMARY KEY,name text NOT NULL,created_at timestamptz NOT NULL DEFAULT now());
INSERT INTO teams(id,name) VALUES('default','Default team') ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS team_members (
 team_id text NOT NULL REFERENCES teams(id),user_id text NOT NULL REFERENCES users(id),
 role text NOT NULL CHECK(role IN ('owner','admin','member')),
 grants jsonb NOT NULL DEFAULT '[]',PRIMARY KEY(team_id,user_id)
);
CREATE TABLE IF NOT EXISTS team_nodes (
 team_id text NOT NULL REFERENCES teams(id),node_id text NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 PRIMARY KEY(team_id,node_id)
);
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS user_id text NOT NULL DEFAULT 'bootstrap' REFERENCES users(id);
CREATE INDEX IF NOT EXISTS sessions_user ON sessions(user_id);
ALTER TABLE projects ADD COLUMN IF NOT EXISTS team_id text NOT NULL DEFAULT 'default' REFERENCES teams(id);
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS projects_team_name ON projects(team_id,name);
CREATE UNIQUE INDEX IF NOT EXISTS projects_id_team ON projects(id,team_id);
ALTER TABLE environments ADD COLUMN IF NOT EXISTS team_id text NOT NULL DEFAULT 'default' REFERENCES teams(id);
CREATE UNIQUE INDEX IF NOT EXISTS environments_id_team ON environments(id,team_id);
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS team_id text NOT NULL DEFAULT 'default' REFERENCES teams(id);
CREATE INDEX IF NOT EXISTS workloads_team ON workloads(team_id);
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS user_id text NOT NULL DEFAULT 'bootstrap' REFERENCES users(id);
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS team_id text NOT NULL DEFAULT 'default' REFERENCES teams(id);
ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS team_id text REFERENCES teams(id);
ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS registration_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS integration_settings bytea;
ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS setup_completed boolean NOT NULL DEFAULT false;
CREATE TABLE IF NOT EXISTS invitations (
 id text PRIMARY KEY,token_hash bytea UNIQUE NOT NULL,team_id text NOT NULL REFERENCES teams(id),
 email text NOT NULL,role text NOT NULL CHECK(role IN ('admin','member')),grants jsonb NOT NULL,
 created_by text NOT NULL REFERENCES users(id),expires_at timestamptz NOT NULL,consumed_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS account_actions (
 token_hash bytea PRIMARY KEY,purpose text NOT NULL CHECK(purpose IN ('register','reset','email')),
 user_id text REFERENCES users(id),email text NOT NULL,name text NOT NULL DEFAULT '',password_hash bytea,
 expires_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS account_actions_user ON account_actions(user_id,purpose);
CREATE TABLE IF NOT EXISTS oauth_states (
 token_hash bytea PRIMARY KEY,browser_hash bytea NOT NULL,verifier bytea NOT NULL,
 link_user_id text REFERENCES users(id),link_version bigint,invitation_id text REFERENCES invitations(id) ON DELETE CASCADE,
 expires_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS schema_migrations (id text PRIMARY KEY);
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM schema_migrations WHERE id='team-accounts-v1') THEN
  INSERT INTO team_members(team_id,user_id,role) VALUES('default','bootstrap','owner');
  INSERT INTO team_nodes(team_id,node_id) SELECT 'default',id FROM nodes;
  ALTER TABLE environments ADD CONSTRAINT environments_project_team_fkey FOREIGN KEY(project_id,team_id) REFERENCES projects(id,team_id) ON DELETE CASCADE;
  ALTER TABLE workloads ADD CONSTRAINT workloads_environment_team_fkey FOREIGN KEY(environment_id,team_id) REFERENCES environments(id,team_id);
  INSERT INTO schema_migrations(id) VALUES('team-accounts-v1');
 END IF;
END $$;

CREATE OR REPLACE FUNCTION cove_access(actor text,team text,project text,environment text) RETURNS integer
LANGUAGE sql STABLE AS $$
 SELECT COALESCE(max(CASE WHEN m.role IN ('owner','admin') THEN 3 ELSE
  CASE g->>'role' WHEN 'editor' THEN 3 WHEN 'operator' THEN 2 WHEN 'viewer' THEN 1 ELSE 0 END END),0)::integer
 FROM team_members m LEFT JOIN LATERAL jsonb_array_elements(m.grants) g ON true
 WHERE m.user_id=actor AND m.team_id=team AND (m.role IN ('owner','admin') OR
  g->>'scope'='team' OR (g->>'scope'='project' AND g->>'scope_id'=project) OR
  (g->>'scope'='environment' AND g->>'scope_id'=environment))
$$;

CREATE INDEX IF NOT EXISTS sessions_expiry ON sessions(expires_at);
CREATE INDEX IF NOT EXISTS account_actions_expiry ON account_actions(expires_at);
CREATE INDEX IF NOT EXISTS oauth_states_expiry ON oauth_states(expires_at);

CREATE TABLE IF NOT EXISTS shared_networks (
 id text PRIMARY KEY,
 team_id text NOT NULL REFERENCES teams(id),
 node_id text NOT NULL REFERENCES nodes(id),
 name text NOT NULL,
 UNIQUE(team_id,node_id,name)
);
CREATE TABLE IF NOT EXISTS workload_networks (
 workload_id text NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
 network_id text NOT NULL REFERENCES shared_networks(id),
 service text NOT NULL,
 alias text NOT NULL,
 PRIMARY KEY(workload_id,network_id,service),
 UNIQUE(network_id,alias)
);
CREATE TABLE IF NOT EXISTS github_app (
 id integer PRIMARY KEY CHECK(id=1), app_id bigint NOT NULL, slug text NOT NULL,
 credentials bytea NOT NULL
);
CREATE TABLE IF NOT EXISTS github_app_setup (
 state_hash bytea PRIMARY KEY, browser_hash bytea NOT NULL, user_id text NOT NULL, user_version bigint NOT NULL, expires_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS github_installations (
 id bigint PRIMARY KEY, account text NOT NULL, team_id text REFERENCES teams(id) ON DELETE SET NULL
);
CREATE TABLE IF NOT EXISTS github_installation_managers (
 installation_id bigint NOT NULL REFERENCES github_installations(id) ON DELETE CASCADE,
 user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 PRIMARY KEY(installation_id,user_id)
);
CREATE TABLE IF NOT EXISTS github_installation_teams (
 installation_id bigint NOT NULL REFERENCES github_installations(id) ON DELETE CASCADE,
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 PRIMARY KEY(installation_id,team_id)
);
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM schema_migrations WHERE id='github-team-sharing-v1') THEN
  INSERT INTO github_installation_teams(installation_id,team_id) SELECT id,team_id FROM github_installations WHERE team_id IS NOT NULL ON CONFLICT DO NOTHING;
  INSERT INTO schema_migrations(id) VALUES('github-team-sharing-v1');
 END IF;
END $$;
CREATE TABLE IF NOT EXISTS github_installation_states (
 state_hash bytea PRIMARY KEY, browser_hash bytea NOT NULL, verifier bytea NOT NULL,
 user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE, user_version bigint NOT NULL,
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE, account text NOT NULL,
 expires_at timestamptz NOT NULL
);
ALTER TABLE github_installation_states ADD COLUMN IF NOT EXISTS session_hash bytea;
ALTER TABLE github_installation_states ALTER COLUMN team_id DROP NOT NULL;
CREATE TABLE IF NOT EXISTS github_sources (
 workload_id text PRIMARY KEY REFERENCES workloads(id) ON DELETE CASCADE,
 installation_id bigint NOT NULL REFERENCES github_installations(id), repository_id bigint NOT NULL,
 repository text NOT NULL, automatic boolean NOT NULL DEFAULT true
);

ALTER TABLE workloads ADD COLUMN IF NOT EXISTS project_id text;
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM schema_migrations WHERE id='resource-project-placement-v1') THEN
  UPDATE workloads w SET project_id=e.project_id FROM environments e WHERE w.environment_id=e.id;
  INSERT INTO projects(id,name,team_id)
   SELECT md5('cove-imported-resources:'||team_id),'Imported resources',team_id
   FROM workloads WHERE project_id IS NULL GROUP BY team_id
   ON CONFLICT(team_id,name) DO NOTHING;
  UPDATE workloads w SET project_id=p.id FROM projects p
   WHERE w.project_id IS NULL AND p.team_id=w.team_id AND p.name='Imported resources';
  ALTER TABLE workloads ALTER COLUMN project_id SET NOT NULL;
  ALTER TABLE workloads ADD CONSTRAINT workloads_project_team_fkey
   FOREIGN KEY(project_id,team_id) REFERENCES projects(id,team_id);
  CREATE UNIQUE INDEX environments_id_project_team ON environments(id,project_id,team_id);
  ALTER TABLE workloads ADD CONSTRAINT workloads_environment_project_fkey
   FOREIGN KEY(environment_id,project_id,team_id) REFERENCES environments(id,project_id,team_id);
  CREATE INDEX workloads_project ON workloads(project_id);
  INSERT INTO schema_migrations(id) VALUES('resource-project-placement-v1');
 END IF;
END $$;

ALTER TABLE workloads ADD COLUMN IF NOT EXISTS manually_stopped boolean NOT NULL DEFAULT false;
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM schema_migrations WHERE id='manual-stop-intent-v1') THEN
  UPDATE workloads w SET manually_stopped=true WHERE (
   SELECT action FROM operations WHERE workload_id=w.id
    AND action IN ('stop','start','restart','deploy','rollback')
    ORDER BY created_at DESC LIMIT 1
  )='stop';
  INSERT INTO schema_migrations(id) VALUES('manual-stop-intent-v1');
 END IF;
END $$;

CREATE TABLE IF NOT EXISTS terminal_sessions (
 id text PRIMARY KEY, node_id text NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 user_id text NOT NULL, team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 expires_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS infrastructure_connections (
 id text PRIMARY KEY, team_id text NOT NULL REFERENCES teams(id), kind text NOT NULL CHECK(kind IN ('registry','storage','notification')),
 name text NOT NULL, configuration bytea NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(team_id,kind,name)
);
CREATE TABLE IF NOT EXISTS workload_connections (
 workload_id text NOT NULL REFERENCES workloads(id) ON DELETE CASCADE, kind text NOT NULL CHECK(kind IN ('registry','storage')),
 connection_id text NOT NULL REFERENCES infrastructure_connections(id), PRIMARY KEY(workload_id,kind)
);
CREATE TABLE IF NOT EXISTS notification_deliveries (
 operation_id text NOT NULL REFERENCES operations(id) ON DELETE CASCADE, connection_id text NOT NULL REFERENCES infrastructure_connections(id) ON DELETE CASCADE,
 state text NOT NULL DEFAULT 'pending', attempts integer NOT NULL DEFAULT 0, last_error text NOT NULL DEFAULT '',
 next_attempt timestamptz NOT NULL DEFAULT now(), created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(operation_id,connection_id)
);
CREATE OR REPLACE FUNCTION cove_notify_operation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 INSERT INTO notification_deliveries(operation_id,connection_id)
  SELECT NEW.id,c.id FROM infrastructure_connections c JOIN workloads w ON w.team_id=c.team_id
  WHERE w.id=NEW.workload_id AND c.kind='notification' AND c.created_at<=NEW.created_at ON CONFLICT DO NOTHING;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS cove_operation_notification ON operations;
CREATE TRIGGER cove_operation_notification AFTER UPDATE OF state ON operations FOR EACH ROW
 WHEN (OLD.state IS DISTINCT FROM NEW.state AND NEW.state IN ('succeeded','failed','interrupted','cancelled')) EXECUTE FUNCTION cove_notify_operation();

CREATE INDEX IF NOT EXISTS notification_deliveries_due ON notification_deliveries(next_attempt) WHERE state='pending';

CREATE TABLE IF NOT EXISTS cloudflare_zones (
 zone_id text PRIMARY KEY, team_id text NOT NULL REFERENCES teams(id), domain text NOT NULL UNIQUE,
 configuration bytea NOT NULL, last_error text NOT NULL DEFAULT '', checked_at timestamptz
);

ALTER TABLE cloudflare_zones ADD COLUMN IF NOT EXISTS owner_id text REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE cloudflare_zones ALTER COLUMN team_id DROP NOT NULL;
CREATE TABLE IF NOT EXISTS cloudflare_zone_teams (
 zone_id text NOT NULL REFERENCES cloudflare_zones(zone_id) ON DELETE CASCADE,
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 domains text[] NOT NULL,
 PRIMARY KEY(zone_id,team_id)
);
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM schema_migrations WHERE id='cloudflare-user-sharing-v1') THEN
  UPDATE cloudflare_zones SET owner_id='bootstrap';
  INSERT INTO cloudflare_zone_teams(zone_id,team_id,domains)
   SELECT zone_id,team_id,ARRAY[domain,'*.'||domain] FROM cloudflare_zones WHERE team_id IS NOT NULL;
  UPDATE cloudflare_zones SET team_id=NULL;
  INSERT INTO schema_migrations(id) VALUES('cloudflare-user-sharing-v1');
 END IF;
END $$;

CREATE TABLE IF NOT EXISTS operation_dependencies (
 operation_id text PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
 prerequisite_id text NOT NULL REFERENCES operations(id), CHECK(operation_id<>prerequisite_id)
);

CREATE TABLE IF NOT EXISTS deployment_sequences (
 project_id text PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 workload_ids jsonb NOT NULL, automatic boolean NOT NULL DEFAULT false
);
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS prerequisite_workload text;
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS prerequisite_delivery text;

CREATE TABLE IF NOT EXISTS health_alerts (
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 kind text NOT NULL, subject text NOT NULL, message text NOT NULL,
 first_seen timestamptz NOT NULL DEFAULT now(), checked_at timestamptz NOT NULL DEFAULT now(),
 active boolean NOT NULL DEFAULT false, last_notified timestamptz,
 PRIMARY KEY(team_id,kind,subject)
);
CREATE TABLE IF NOT EXISTS health_deliveries (
 id text PRIMARY KEY, team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 connection_id text NOT NULL REFERENCES infrastructure_connections(id) ON DELETE CASCADE,
 event text NOT NULL, message text NOT NULL,
 state text NOT NULL DEFAULT 'pending', attempts integer NOT NULL DEFAULT 0,
 next_attempt timestamptz NOT NULL DEFAULT now(), created_at timestamptz NOT NULL DEFAULT now(),
 last_error text NOT NULL DEFAULT ''
);
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS backup_policy_since timestamptz NOT NULL DEFAULT now();
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS health_checked_at timestamptz;

ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS superseded_by text;

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS id text NOT NULL DEFAULT gen_random_uuid()::text;
CREATE UNIQUE INDEX IF NOT EXISTS session_ids ON sessions(id);
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS user_agent text NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS shared_variables (
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 project_id text REFERENCES projects(id) ON DELETE CASCADE,
 environment_id text REFERENCES environments(id) ON DELETE CASCADE,
 name text NOT NULL,
 value bytea NOT NULL,
 CHECK (environment_id IS NULL OR project_id IS NOT NULL),
 UNIQUE NULLS NOT DISTINCT(team_id,project_id,environment_id,name)
);

CREATE TABLE IF NOT EXISTS configuration_history (
 id bigserial PRIMARY KEY,
 workload_id text NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
 configuration bytea NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS configuration_history_resource ON configuration_history(workload_id,id DESC);
CREATE OR REPLACE FUNCTION cove_configuration_history() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='INSERT' OR OLD.configuration IS DISTINCT FROM NEW.configuration THEN
  INSERT INTO configuration_history(workload_id,configuration) VALUES(NEW.id,NEW.configuration);
  DELETE FROM configuration_history WHERE workload_id=NEW.id AND id NOT IN (SELECT id FROM configuration_history WHERE workload_id=NEW.id ORDER BY id DESC LIMIT 100);
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS record_configuration_history ON workloads;
CREATE TRIGGER record_configuration_history AFTER INSERT OR UPDATE OF configuration ON workloads FOR EACH ROW EXECUTE FUNCTION cove_configuration_history();
INSERT INTO configuration_history(workload_id,configuration) SELECT id,configuration FROM workloads w WHERE NOT EXISTS(SELECT 1 FROM configuration_history h WHERE h.workload_id=w.id);

ALTER TABLE workloads ADD COLUMN IF NOT EXISTS drill_interval integer NOT NULL DEFAULT 0;
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS next_drill_at timestamptz;
CREATE INDEX IF NOT EXISTS due_drills ON workloads(next_drill_at) WHERE drill_interval>0;
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS job_cron text NOT NULL DEFAULT '';
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS next_job_at timestamptz;
CREATE INDEX IF NOT EXISTS due_jobs ON workloads(next_job_at) WHERE job_cron<>'';
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS configured_cpu double precision;
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS configured_memory_mb bigint;
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='operations' AND column_name='admitted_at') THEN
  ALTER TABLE operations ADD COLUMN admitted_at timestamptz;
  UPDATE operations SET admitted_at=created_at WHERE state IN ('queued','running');
 END IF;
END $$;
CREATE TABLE IF NOT EXISTS team_quotas (
 team_id text PRIMARY KEY REFERENCES teams(id) ON DELETE CASCADE,
 resources integer NOT NULL DEFAULT 0 CHECK(resources>=0),
 cpus double precision NOT NULL DEFAULT 0 CHECK(cpus>=0),
 memory_mb bigint NOT NULL DEFAULT 0 CHECK(memory_mb>=0),
 deployments integer NOT NULL DEFAULT 0 CHECK(deployments>=0)
);
CREATE OR REPLACE FUNCTION cove_enforce_team_quota() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE quota team_quotas; used_count bigint; used_cpu double precision; used_memory bigint;
BEGIN
 PERFORM pg_advisory_xact_lock(hashtextextended('cove-quota:'||NEW.team_id,0));
 SELECT * INTO quota FROM team_quotas WHERE team_id=NEW.team_id;
 IF NOT FOUND THEN RETURN NEW; END IF;
 SELECT count(*),COALESCE(sum(configured_cpu),0),COALESCE(sum(configured_memory_mb),0) INTO used_count,used_cpu,used_memory FROM workloads WHERE team_id=NEW.team_id AND id<>NEW.id;
 IF (quota.resources>0 AND used_count+1>quota.resources) OR (quota.cpus>0 AND used_cpu+COALESCE(NEW.configured_cpu,0)>quota.cpus+0.000001) OR (quota.memory_mb>0 AND used_memory+COALESCE(NEW.configured_memory_mb,0)>quota.memory_mb) THEN
  RAISE EXCEPTION 'team quota exceeded' USING ERRCODE='23514',CONSTRAINT='cove_team_quota';
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS enforce_team_quota ON workloads;
CREATE TRIGGER enforce_team_quota BEFORE INSERT OR UPDATE OF configured_cpu,configured_memory_mb,team_id ON workloads FOR EACH ROW EXECUTE FUNCTION cove_enforce_team_quota();
CREATE TABLE IF NOT EXISTS deployment_policies (
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 project_id text REFERENCES projects(id) ON DELETE CASCADE,
 environment_id text REFERENCES environments(id) ON DELETE CASCADE,
 approval boolean NOT NULL DEFAULT false,
 freeze_from timestamptz,
 freeze_until timestamptz,
 CHECK((project_id IS NULL)<>(environment_id IS NULL)),
 UNIQUE NULLS NOT DISTINCT(team_id,project_id,environment_id)
);
ALTER TABLE operations ADD COLUMN IF NOT EXISTS approval_required boolean NOT NULL DEFAULT false;
ALTER TABLE operations ADD COLUMN IF NOT EXISTS approved_by text;
ALTER TABLE operations ADD COLUMN IF NOT EXISTS approved_at timestamptz;
CREATE OR REPLACE FUNCTION cove_deployment_approval() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.action IN ('deploy','rollback') THEN
  SELECT EXISTS(SELECT 1 FROM workloads w JOIN deployment_policies p ON p.team_id=w.team_id AND (p.project_id=w.project_id OR p.environment_id=w.environment_id) WHERE w.id=NEW.workload_id AND p.approval) INTO NEW.approval_required;
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS deployment_approval ON operations;
CREATE TRIGGER deployment_approval BEFORE INSERT ON operations FOR EACH ROW EXECUTE FUNCTION cove_deployment_approval();
CREATE TABLE IF NOT EXISTS resource_variable_bindings (
 workload_id text NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
 name text NOT NULL,
 source_id text NOT NULL REFERENCES workloads(id) ON DELETE RESTRICT,
 source_key text NOT NULL,
 PRIMARY KEY(workload_id,name),
 CHECK(workload_id<>source_id)
);

CREATE TABLE IF NOT EXISTS database_access (
 id text PRIMARY KEY,
 workload_id text NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
 node_id text NOT NULL REFERENCES nodes(id),
 user_id text NOT NULL,
 service text NOT NULL,
 port integer NOT NULL CHECK(port BETWEEN 1 AND 65535),
 expires_at timestamptz NOT NULL,
 revoked_at timestamptz
);

CREATE TABLE IF NOT EXISTS preview_policies (
 workload_id text PRIMARY KEY REFERENCES workloads(id) ON DELETE CASCADE,
 configuration bytea NOT NULL
);
CREATE TABLE IF NOT EXISTS previews (
 source_id text NOT NULL REFERENCES workloads(id) ON DELETE RESTRICT,
 number integer NOT NULL CHECK(number>0),
 workload_id text UNIQUE NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
 environment_id text NOT NULL REFERENCES environments(id),
 revision text NOT NULL,
 deployed_revision text NOT NULL DEFAULT '',
 expires_at timestamptz NOT NULL,
 closing boolean NOT NULL DEFAULT false,
 cleanup_operation text NOT NULL DEFAULT '',
 PRIMARY KEY(source_id,number)
);
CREATE TABLE IF NOT EXISTS preview_deliveries (
 delivery text PRIMARY KEY,
 received_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS preview_pull_requests (
 source_id text NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
 number integer NOT NULL,
 updated_at timestamptz NOT NULL,
 closed boolean NOT NULL,
 PRIMARY KEY(source_id,number)
);
ALTER TABLE previews ADD COLUMN IF NOT EXISTS retry_after timestamptz NOT NULL DEFAULT now();
ALTER TABLE deployment_policies ADD COLUMN IF NOT EXISTS window_start text NOT NULL DEFAULT '';
ALTER TABLE deployment_policies ADD COLUMN IF NOT EXISTS window_end text NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS cloudflare_accounts (
 id text PRIMARY KEY,
 owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
 name text NOT NULL,
 kind text NOT NULL CHECK(kind IN ('token','oauth')),
 credentials bytea NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS cloudflare_oauth_client (
 id integer PRIMARY KEY CHECK(id=1),
 credentials bytea NOT NULL
);
CREATE TABLE IF NOT EXISTS cloudflare_oauth_states (
 state_hash bytea PRIMARY KEY,
 browser_hash bytea NOT NULL,
 verifier bytea NOT NULL,
 user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 user_version bigint NOT NULL,
 session_hash bytea NOT NULL,
 client_id text NOT NULL,
 expires_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS resource_connections (
 id text PRIMARY KEY,
 team_id text NOT NULL REFERENCES teams(id),
 source_id text NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
 source_service text NOT NULL,
 target_id text NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
 target_service text NOT NULL,
 port integer NOT NULL CHECK(port BETWEEN 1 AND 65535),
 alias text NOT NULL,
 version bigint NOT NULL DEFAULT 1,
 removing boolean NOT NULL DEFAULT false,
 created_by text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK(source_id<>target_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS resource_connections_endpoint ON resource_connections(source_id,source_service,alias,port) WHERE NOT removing;
CREATE UNIQUE INDEX IF NOT EXISTS resource_connections_pair ON resource_connections(source_id,source_service,target_id,target_service,port) WHERE NOT removing;
CREATE INDEX IF NOT EXISTS resource_connections_target ON resource_connections(target_id);
CREATE TABLE IF NOT EXISTS resource_connection_status (
 connection_id text NOT NULL REFERENCES resource_connections(id) ON DELETE CASCADE,
 node_id text NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 role text NOT NULL CHECK(role IN ('source','target')),
 version bigint NOT NULL,
 state text NOT NULL CHECK(state IN ('sending','applied','error')),
 detail text NOT NULL DEFAULT '',
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(connection_id,node_id,role)
);
CREATE TABLE IF NOT EXISTS node_links (
 node_id text PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
 fingerprint text NOT NULL DEFAULT '',
 link_address text NOT NULL DEFAULT '',
 link_port integer NOT NULL DEFAULT 47800 CHECK(link_port BETWEEN 1 AND 65535),
 applied_digest text NOT NULL DEFAULT '',
 applied_at timestamptz,
 last_error text NOT NULL DEFAULT '',
 failures integer NOT NULL DEFAULT 0,
 next_attempt_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS preview_recipes (
 project_id text PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 team_id text NOT NULL REFERENCES teams(id),
 base_environment_id text NOT NULL REFERENCES environments(id),
 enabled boolean NOT NULL DEFAULT false,
 configuration bytea NOT NULL,
 version bigint NOT NULL DEFAULT 1,
 updated_at timestamptz NOT NULL DEFAULT now(),
 next_scan_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS environment_previews (
 id text PRIMARY KEY,
 project_id text NOT NULL REFERENCES projects(id),
 team_id text NOT NULL REFERENCES teams(id),
 base_environment_id text NOT NULL REFERENCES environments(id),
 environment_id text UNIQUE REFERENCES environments(id),
 suffix text NOT NULL,
 slug text NOT NULL,
 state text NOT NULL CHECK(state IN ('awaiting_approval','creating','ready','updating','failed','deleting')),
 stage text NOT NULL CHECK(stage IN ('approval','plan','data','connect','deploy','publish','ready','remove')),
 detail text NOT NULL DEFAULT '',
 configuration bytea NOT NULL,
 recipe_version bigint NOT NULL,
 pinned boolean NOT NULL DEFAULT false,
 delete_reason text NOT NULL DEFAULT '',
 refresh_scope text NOT NULL DEFAULT '',
 created_by text NOT NULL DEFAULT '',
 last_activity_at timestamptz NOT NULL DEFAULT now(),
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(project_id,suffix),
 UNIQUE(project_id,slug)
);
CREATE INDEX IF NOT EXISTS environment_previews_due ON environment_previews(next_attempt_at);
CREATE TABLE IF NOT EXISTS environment_preview_resources (
 preview_id text NOT NULL REFERENCES environment_previews(id) ON DELETE CASCADE,
 base_id text NOT NULL,
 resource_id text UNIQUE NOT NULL REFERENCES workloads(id),
 name text NOT NULL,
 kind text NOT NULL,
 position integer NOT NULL DEFAULT 0,
 repository_id bigint NOT NULL DEFAULT 0,
 baseline bytea NOT NULL,
 baseline_revision text NOT NULL DEFAULT '',
 baseline_release text NOT NULL DEFAULT '',
 baseline_deployed_at timestamptz,
 branch_revision text NOT NULL DEFAULT '',
 deployed_key text NOT NULL DEFAULT '',
 isolation jsonb NOT NULL,
 data_state text NOT NULL DEFAULT 'pending' CHECK(data_state IN ('pending','copying','ready','failed','none')),
 data_operation text NOT NULL DEFAULT '',
 data_report jsonb,
 snapshot_at timestamptz,
 deploy_operation text NOT NULL DEFAULT '',
 deploying_key text NOT NULL DEFAULT '',
 routes_operation text NOT NULL DEFAULT '',
 remove_operation text NOT NULL DEFAULT '',
 routes jsonb NOT NULL DEFAULT '[]',
 PRIMARY KEY(preview_id,base_id)
);
CREATE TABLE IF NOT EXISTS environment_preview_branches (
 preview_id text NOT NULL REFERENCES environment_previews(id) ON DELETE CASCADE,
 repository_id bigint NOT NULL,
 repository text NOT NULL,
 branch text NOT NULL,
 head text NOT NULL DEFAULT '',
 checked_at timestamptz,
 PRIMARY KEY(preview_id,repository_id)
);
CREATE TABLE IF NOT EXISTS environment_preview_networks (
 preview_id text NOT NULL REFERENCES environment_previews(id) ON DELETE CASCADE,
 base_network_id text NOT NULL,
 network_id text NOT NULL REFERENCES shared_networks(id),
 PRIMARY KEY(preview_id,base_network_id)
);
CREATE TABLE IF NOT EXISTS preview_branch_signals (
 project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 repository_id bigint NOT NULL,
 branch text NOT NULL,
 reason text NOT NULL DEFAULT '',
 approved_by text NOT NULL DEFAULT '',
 attempts integer NOT NULL DEFAULT 0,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 received_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(project_id,repository_id,branch)
);
CREATE TABLE IF NOT EXISTS preview_branch_deliveries (
 delivery text PRIMARY KEY,
 received_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS preview_branch_deliveries_age ON preview_branch_deliveries(received_at);
ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS require_second_factor boolean NOT NULL DEFAULT false;
ALTER TABLE teams ADD COLUMN IF NOT EXISTS require_second_factor boolean NOT NULL DEFAULT false;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS second_factor_verified boolean NOT NULL DEFAULT false;
CREATE TABLE IF NOT EXISTS account_totp (
 user_id text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 secret bytea NOT NULL,
 confirmed boolean NOT NULL DEFAULT false,
 expires_at timestamptz NOT NULL DEFAULT now()+interval '10 minutes',
 last_step bigint NOT NULL DEFAULT -1
);
CREATE TABLE IF NOT EXISTS recovery_codes (
 user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 code_hash bytea NOT NULL,
 PRIMARY KEY(user_id,code_hash)
);
-- Branch heads of removed previews. The periodic scan must not bring back a preview that was deleted
-- or expired; only a new head or an explicit request starts one again.
CREATE TABLE IF NOT EXISTS preview_branch_dismissals (
 project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 repository_id bigint NOT NULL,
 branch text NOT NULL,
 head text NOT NULL,
 dismissed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(project_id,repository_id,branch)
);

-- A preview environment grants exactly the access its production environment grants.
CREATE OR REPLACE FUNCTION cove_access(actor text,team text,project text,environment text) RETURNS integer
LANGUAGE sql STABLE AS $$
 SELECT COALESCE(max(CASE WHEN m.role IN ('owner','admin') THEN 3 ELSE
  CASE g->>'role' WHEN 'editor' THEN 3 WHEN 'operator' THEN 2 WHEN 'viewer' THEN 1 ELSE 0 END END),0)::integer
 FROM team_members m LEFT JOIN LATERAL jsonb_array_elements(m.grants) g ON true
 CROSS JOIN LATERAL (SELECT COALESCE((SELECT p.base_environment_id FROM environment_previews p WHERE p.environment_id=environment AND p.team_id=team),environment) AS scope) s
 WHERE m.user_id=actor AND m.team_id=team AND (m.role IN ('owner','admin') OR
  g->>'scope'='team' OR (g->>'scope'='project' AND g->>'scope_id'=project) OR
  (g->>'scope'='environment' AND g->>'scope_id'=s.scope))
$$;

CREATE TABLE IF NOT EXISTS github_reporting (
 workload_id text PRIMARY KEY REFERENCES workloads(id) ON DELETE CASCADE,
 enabled boolean NOT NULL DEFAULT false,
 enabled_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS github_operation_feedback (
 operation_id text PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
 revision text NOT NULL,
 reported_state text NOT NULL DEFAULT '',
 deployment_id bigint NOT NULL DEFAULT 0,
 attempts integer NOT NULL DEFAULT 0,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 error text NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS git_deploy_keys (
 workload_id text PRIMARY KEY REFERENCES workloads(id) ON DELETE CASCADE,
 repository text NOT NULL,
 host_key text NOT NULL,
 public_key text NOT NULL,
 private_key bytea NOT NULL
);

CREATE TABLE IF NOT EXISTS git_connections (
 id bigserial PRIMARY KEY,
 team_id text NOT NULL REFERENCES teams(id),
 configuration bytea NOT NULL
);
CREATE TABLE IF NOT EXISTS git_repositories (
 id bigserial PRIMARY KEY,
 connection_id bigint NOT NULL REFERENCES git_connections(id) ON DELETE CASCADE,
 external_id text NOT NULL,
 full_name text NOT NULL,
 clone_url text NOT NULL,
 default_branch text NOT NULL,
 UNIQUE(connection_id,external_id),
 UNIQUE(id,connection_id)
);
CREATE TABLE IF NOT EXISTS git_sources (
 workload_id text PRIMARY KEY REFERENCES workloads(id) ON DELETE CASCADE,
 connection_id bigint NOT NULL REFERENCES git_connections(id),
 repository_id bigint NOT NULL,
 automatic boolean NOT NULL DEFAULT false,
 reporting boolean NOT NULL DEFAULT false,
 connected_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(repository_id,connection_id) REFERENCES git_repositories(id,connection_id)
);
CREATE TABLE IF NOT EXISTS git_provider_feedback (
 operation_id text PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
 reported_state text NOT NULL DEFAULT '',
 attempts integer NOT NULL DEFAULT 0,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 error text NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS git_source_triggers (
 workload_id text PRIMARY KEY REFERENCES workloads(id) ON DELETE CASCADE,
 kind text NOT NULL CHECK(kind IN ('branch','tags','releases')),
 pattern text NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS deploy_hooks (
 id text PRIMARY KEY,
 workload_id text UNIQUE NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
 token_hash bytea UNIQUE NOT NULL,
 creator_id text NOT NULL REFERENCES users(id),
 expires_at timestamptz NOT NULL,
 window_start timestamptz NOT NULL DEFAULT now(),
 window_count integer NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS deploy_hook_receipts (
 hook_id text NOT NULL REFERENCES deploy_hooks(id) ON DELETE CASCADE,
 delivery text NOT NULL,
 request_id text NOT NULL,
 queue_delivery text NOT NULL,
 cancelled boolean NOT NULL DEFAULT false,
 PRIMARY KEY(hook_id,delivery)
);

CREATE TABLE IF NOT EXISTS secret_providers (
    id text PRIMARY KEY,
    team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    configuration bytea NOT NULL
);
CREATE INDEX IF NOT EXISTS secret_providers_team ON secret_providers(team_id);
CREATE TABLE IF NOT EXISTS access_tokens (
 id text PRIMARY KEY,
 name text NOT NULL,
 user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 team_id text REFERENCES teams(id) ON DELETE CASCADE,
 scopes text[] NOT NULL,
 token_hash bytea NOT NULL UNIQUE,
 expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS access_tokens_owner ON access_tokens(user_id);
ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS mcp_enabled boolean NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS configuration_resource_keys (
 project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 key text NOT NULL,
 workload_id text NOT NULL UNIQUE REFERENCES workloads(id) ON DELETE CASCADE,
 PRIMARY KEY(project_id,key)
);
CREATE TABLE IF NOT EXISTS configuration_drafts (
 id text PRIMARY KEY,
 project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 creator_id text NOT NULL,
 payload bytea NOT NULL,
 base_version text NOT NULL,
 changes jsonb NOT NULL,
 operation_ids jsonb NOT NULL DEFAULT '[]',
 created_at timestamptz NOT NULL DEFAULT now(),
 applied_at timestamptz
);
CREATE INDEX IF NOT EXISTS configuration_drafts_project ON configuration_drafts(project_id,created_at DESC);

ALTER TABLE operations ADD COLUMN IF NOT EXISTS declared_payload bytea;
CREATE OR REPLACE FUNCTION cove_declared_operation_payload() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 NEW.declared_payload=NEW.payload;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS declared_operation_payload ON operations;
CREATE TRIGGER declared_operation_payload BEFORE INSERT ON operations FOR EACH ROW EXECUTE FUNCTION cove_declared_operation_payload();

CREATE TABLE IF NOT EXISTS configuration_gitops (
 project_id text PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 workload_id text NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
 path text NOT NULL DEFAULT 'cove.yaml',
 automatic boolean NOT NULL DEFAULT false,
 creator_id text NOT NULL,
 last_revision text NOT NULL DEFAULT '',
 desired_payload bytea,
 last_draft_id text REFERENCES configuration_drafts(id) ON DELETE SET NULL,
 last_error text NOT NULL DEFAULT '',
 updated_at timestamptz NOT NULL DEFAULT now(),
 synced_at timestamptz,
 next_scan_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS configuration_gitops_due ON configuration_gitops(next_scan_at) WHERE automatic;

ALTER TABLE operations ADD COLUMN IF NOT EXISTS drift_prepared boolean NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS migration_imports (
 id text PRIMARY KEY,
 project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 payload bytea NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL DEFAULT now()+interval '24 hours'
);
CREATE INDEX IF NOT EXISTS migration_imports_project ON migration_imports(project_id,expires_at);

CREATE TABLE IF NOT EXISTS compose_adoptions (
 id text PRIMARY KEY,
 node_id text NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 project_id text NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 environment_id text REFERENCES environments(id) ON DELETE CASCADE,
 payload bytea NOT NULL,
 base_version text NOT NULL,
 resource_ids jsonb NOT NULL,
 completed jsonb NOT NULL DEFAULT '[]',
 status text NOT NULL DEFAULT 'planned' CHECK(status IN ('planned','applying','complete')),
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL DEFAULT now()+interval '24 hours'
);
CREATE INDEX IF NOT EXISTS compose_adoptions_node ON compose_adoptions(node_id,team_id,created_at);
CREATE TABLE IF NOT EXISTS compose_adoption_claims (
 node_id text NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 project_name text NOT NULL,
 adoption_id text NOT NULL REFERENCES compose_adoptions(id) ON DELETE CASCADE,
 workload_id text NOT NULL UNIQUE REFERENCES workloads(id) ON DELETE CASCADE,
 PRIMARY KEY(node_id,project_name)
);
CREATE OR REPLACE FUNCTION cove_pending_adoption() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE resource text;
BEGIN
 IF TG_TABLE_NAME='workloads' THEN
  IF current_setting('cove.adoption_write',true)='true' THEN RETURN NEW; END IF;
  resource=NEW.id;
 ELSE
  IF NEW.state NOT IN ('queued','running') THEN RETURN NEW; END IF;
  resource=NEW.workload_id;
 END IF;
 IF EXISTS(SELECT 1 FROM compose_adoptions a WHERE a.status='applying' AND a.resource_ids ? resource) THEN
  RAISE EXCEPTION 'complete pending adoption first' USING ERRCODE='23514',CONSTRAINT='cove_pending_adoption';
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS pending_adoption_workload ON workloads;
CREATE TRIGGER pending_adoption_workload BEFORE UPDATE OF configuration,node_id,team_id,project_id,environment_id ON workloads FOR EACH ROW EXECUTE FUNCTION cove_pending_adoption();
DROP TRIGGER IF EXISTS pending_adoption_operation ON operations;
CREATE TRIGGER pending_adoption_operation BEFORE INSERT ON operations FOR EACH ROW EXECUTE FUNCTION cove_pending_adoption();

-- Workspace resource organization and personal pins.
CREATE TABLE IF NOT EXISTS workload_tags (
 workload_id text PRIMARY KEY REFERENCES workloads(id) ON DELETE CASCADE,
 tags text[] NOT NULL DEFAULT '{}', CHECK(cardinality(tags)<=12)
);
CREATE INDEX IF NOT EXISTS workload_tags_values ON workload_tags USING gin(tags);
CREATE TABLE IF NOT EXISTS user_pins (
 user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 kind text NOT NULL CHECK(kind IN ('project','environment','workload')),
 resource_id text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,team_id,kind,resource_id)
);
CREATE TABLE IF NOT EXISTS user_onboarding (
 user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 dismissed boolean NOT NULL DEFAULT false, PRIMARY KEY(user_id,team_id)
);

-- Private resource notes and display timezone preferences.
CREATE TABLE IF NOT EXISTS project_notes (
 resource_id text PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 content bytea NOT NULL, updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS workload_notes (
 resource_id text PRIMARY KEY REFERENCES workloads(id) ON DELETE CASCADE,
 content bytea NOT NULL, updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE teams ADD COLUMN IF NOT EXISTS display_timezone text NOT NULL DEFAULT 'UTC';
ALTER TABLE users ADD COLUMN IF NOT EXISTS display_timezone text;

-- Explicitly enabled, hash-only public deployment summaries.
CREATE TABLE IF NOT EXISTS project_status_shares (
 project_id text PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 title text NOT NULL, token_hash bytea UNIQUE NOT NULL,
 expires_at timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS passkeys (
 id bytea PRIMARY KEY,
 user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 name text NOT NULL,
 credential jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS webauthn_challenges (
 browser_hash bytea PRIMARY KEY,
 user_id text REFERENCES users(id) ON DELETE CASCADE,
 user_version bigint,
 session_hash bytea,
 ceremony text NOT NULL CHECK(ceremony IN ('register','login')),
 data jsonb NOT NULL,
 expires_at timestamptz NOT NULL DEFAULT now()+interval '5 minutes'
);

ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS password_login_disabled boolean NOT NULL DEFAULT false;
ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS oidc_registration_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS oidc_auto_redirect text NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS oidc_providers (
 id text PRIMARY KEY,
 configuration bytea NOT NULL,
 version bigint NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS oidc_identities (
 provider_id text NOT NULL REFERENCES oidc_providers(id) ON DELETE CASCADE,
 subject text NOT NULL,
 user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 PRIMARY KEY(provider_id,subject),
 UNIQUE(provider_id,user_id)
);
CREATE TABLE IF NOT EXISTS oidc_states (
 token_hash bytea PRIMARY KEY,
 browser_hash bytea NOT NULL,
 provider_id text NOT NULL REFERENCES oidc_providers(id) ON DELETE CASCADE,
 provider_version bigint NOT NULL,
 nonce_hash bytea NOT NULL,
 verifier bytea NOT NULL,
 link_user_id text REFERENCES users(id) ON DELETE CASCADE,
 link_version bigint,
 session_hash bytea,
 expires_at timestamptz NOT NULL DEFAULT now()+interval '10 minutes'
);

CREATE TABLE IF NOT EXISTS login_events (
 id bigserial PRIMARY KEY,
 user_id text REFERENCES users(id) ON DELETE SET NULL,
 success boolean NOT NULL,
 method text NOT NULL,
 address text NOT NULL,
 user_agent text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS login_events_user ON login_events(user_id,id DESC);
CREATE INDEX IF NOT EXISTS login_events_age ON login_events(created_at);
CREATE TABLE IF NOT EXISTS authentication_failures (
 key text PRIMARY KEY,
 failures integer NOT NULL DEFAULT 0,
 window_start timestamptz NOT NULL DEFAULT now(),
 locked_until timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS account_devices (
 user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 device_hash bytea NOT NULL,
 last_seen timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,device_hash)
);

CREATE TABLE IF NOT EXISTS audit_export_settings (
 id integer PRIMARY KEY CHECK(id=1),
 configuration bytea NOT NULL,
 webhook_enabled boolean NOT NULL DEFAULT false,
 syslog_enabled boolean NOT NULL DEFAULT false,
 retention_days integer NOT NULL DEFAULT 365 CHECK(retention_days BETWEEN 1 AND 3650)
);
CREATE TABLE IF NOT EXISTS audit_deliveries (
 id bigserial PRIMARY KEY,
 event_id bigint NOT NULL,
 destination text NOT NULL CHECK(destination IN ('webhook','syslog')),
 configuration bytea NOT NULL,
 payload jsonb NOT NULL,
 attempts integer NOT NULL DEFAULT 0,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(event_id,destination)
);
CREATE INDEX IF NOT EXISTS audit_deliveries_due ON audit_deliveries(next_attempt_at,id);
CREATE OR REPLACE FUNCTION cove_queue_audit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO audit_deliveries(event_id,destination,configuration,payload)
 SELECT NEW.id,d.destination,s.configuration,to_jsonb(NEW)
 FROM audit_export_settings s CROSS JOIN (VALUES ('webhook'),('syslog')) d(destination)
 WHERE (d.destination='webhook' AND s.webhook_enabled) OR (d.destination='syslog' AND s.syslog_enabled);
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS cove_audit_export ON audit_events;
CREATE TRIGGER cove_audit_export AFTER INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION cove_queue_audit();

CREATE TABLE IF NOT EXISTS team_roles (
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 id text NOT NULL,
 name text NOT NULL,
 permissions text[] NOT NULL,
 PRIMARY KEY(team_id,id)
);
CREATE OR REPLACE FUNCTION cove_role_permissions(team text,role text) RETURNS text[] LANGUAGE sql STABLE AS $$
 SELECT CASE role WHEN 'viewer' THEN ARRAY['view'] WHEN 'operator' THEN ARRAY['view','operate','manage_backups']
 WHEN 'editor' THEN ARRAY['view','operate','edit_configuration','read_secrets','manage_backups']
 ELSE COALESCE((SELECT permissions FROM team_roles WHERE team_id=team AND id=role),ARRAY[]::text[]) END
$$;
CREATE OR REPLACE FUNCTION cove_permission(actor text,team text,project text,environment text,permission text) RETURNS boolean
LANGUAGE sql STABLE AS $$
 SELECT COALESCE(bool_or(m.role IN ('owner','admin') OR permission=ANY(cove_role_permissions(team,g->>'role'))),false)
 FROM team_members m LEFT JOIN LATERAL jsonb_array_elements(m.grants) g ON true
 CROSS JOIN LATERAL (SELECT COALESCE((SELECT p.base_environment_id FROM environment_previews p WHERE p.environment_id=environment AND p.team_id=team),environment) AS scope) s
 WHERE m.user_id=actor AND m.team_id=team AND (m.role IN ('owner','admin') OR
 g->>'scope'='team' OR (g->>'scope'='project' AND g->>'scope_id'=project) OR (g->>'scope'='environment' AND g->>'scope_id'=s.scope))
$$;
CREATE OR REPLACE FUNCTION cove_access(actor text,team text,project text,environment text) RETURNS integer LANGUAGE sql STABLE AS $$
 SELECT CASE WHEN cove_permission(actor,team,project,environment,'view') THEN
 CASE WHEN cove_permission(actor,team,project,environment,'edit_configuration') AND cove_permission(actor,team,project,environment,'read_secrets') AND cove_permission(actor,team,project,environment,'manage_backups') AND cove_permission(actor,team,project,environment,'operate') THEN 3
 WHEN cove_permission(actor,team,project,environment,'operate') THEN 2 ELSE 1 END ELSE 0 END
$$;

ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS panel_allowlist text[] NOT NULL DEFAULT '{}';

ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS outbound_proxy bytea;

ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS panel_backup_at timestamptz;
ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS latest_panel_version text NOT NULL DEFAULT '';
ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS panel_update_checked_at timestamptz;
CREATE TABLE IF NOT EXISTS workload_security_observations (
 workload_id text PRIMARY KEY REFERENCES workloads(id) ON DELETE CASCADE,
 observed_at timestamptz NOT NULL DEFAULT now(), exposed boolean NOT NULL
);

ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS panel_update_channel text NOT NULL DEFAULT '';

ALTER TABLE deployment_policies DROP CONSTRAINT IF EXISTS deployment_policies_check;
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='deployment_policies'::regclass AND conname='deployment_policy_scope') THEN
  ALTER TABLE deployment_policies ADD CONSTRAINT deployment_policy_scope CHECK(project_id IS NULL OR environment_id IS NULL);
 END IF;
END $$;
ALTER TABLE deployment_policies ADD COLUMN IF NOT EXISTS timezone text NOT NULL DEFAULT 'UTC';
ALTER TABLE deployment_policies ADD COLUMN IF NOT EXISTS recurring_windows jsonb NOT NULL DEFAULT '[]';
ALTER TABLE deployment_policies ADD COLUMN IF NOT EXISTS freezes jsonb NOT NULL DEFAULT '[]';
ALTER TABLE deployment_policies ADD COLUMN IF NOT EXISTS outside_window text NOT NULL DEFAULT 'block';
ALTER TABLE operations ADD COLUMN IF NOT EXISTS reason text NOT NULL DEFAULT '';
ALTER TABLE operations ADD COLUMN IF NOT EXISTS emergency_override_by text;
ALTER TABLE operations ADD COLUMN IF NOT EXISTS emergency_override_reason text NOT NULL DEFAULT '';
ALTER TABLE operations ADD COLUMN IF NOT EXISTS emergency_override_at timestamptz;
ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS details jsonb NOT NULL DEFAULT '{}';
CREATE OR REPLACE FUNCTION cove_deployment_approval() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.action IN ('deploy','rollback') THEN
  SELECT EXISTS(SELECT 1 FROM workloads w JOIN deployment_policies p ON p.team_id=w.team_id AND ((p.project_id IS NULL AND p.environment_id IS NULL) OR p.project_id=w.project_id OR p.environment_id=w.environment_id) WHERE w.id=NEW.workload_id AND p.approval) INTO NEW.approval_required;
 END IF;
 RETURN NEW;
END $$;

CREATE TABLE IF NOT EXISTS panel_channel_observations (
 channel text PRIMARY KEY CHECK(channel IN ('beta','release')),
 version text NOT NULL, revision text NOT NULL, schema_hash text NOT NULL,
 revision_time timestamptz NOT NULL, checked_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS confirmed_panel_version text NOT NULL DEFAULT '';
ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS confirmed_panel_revision text NOT NULL DEFAULT '';

ALTER TABLE panel_channel_observations ADD COLUMN IF NOT EXISTS source_root text NOT NULL DEFAULT '';
ALTER TABLE panel_channel_observations ADD COLUMN IF NOT EXISTS source_generation bigint NOT NULL DEFAULT 0;

ALTER TABLE workload_security_observations ADD COLUMN IF NOT EXISTS attempted_at timestamptz;
ALTER TABLE workload_security_observations ALTER COLUMN observed_at DROP NOT NULL;

CREATE TABLE IF NOT EXISTS template_repositories (
 id text PRIMARY KEY, team_id text REFERENCES teams(id) ON DELETE CASCADE,
 name text NOT NULL, kind text NOT NULL CHECK(kind IN ('git','index')),
 url text NOT NULL, ref text NOT NULL DEFAULT '', revision text NOT NULL DEFAULT '',
 snapshot jsonb, last_error text NOT NULL DEFAULT '', checked_at timestamptz,
 next_refresh_at timestamptz NOT NULL DEFAULT now(), refresh_until timestamptz NOT NULL DEFAULT '-infinity'
);
CREATE TABLE IF NOT EXISTS team_templates (
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 id text NOT NULL, definition jsonb NOT NULL, revision text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(team_id,id)
);


ALTER TABLE deploy_hook_receipts ADD COLUMN IF NOT EXISTS reason text NOT NULL DEFAULT '';

CREATE OR REPLACE FUNCTION cove_permission_names(actor text,team text,project text,environment text,admin boolean,catalogue text[]) RETURNS text[] LANGUAGE sql STABLE AS $$
 SELECT ARRAY(SELECT permission FROM unnest(catalogue) permission WHERE admin OR cove_permission(actor,team,project,environment,permission))
$$;

CREATE OR REPLACE FUNCTION cove_deployment_environment(resource text) RETURNS text LANGUAGE sql STABLE AS $$
 SELECT COALESCE((SELECT base_environment_id FROM environment_previews p WHERE p.environment_id=w.environment_id AND p.team_id=w.team_id),
 (SELECT s.environment_id FROM previews p JOIN workloads s ON s.id=p.source_id WHERE p.workload_id=w.id AND s.team_id=w.team_id),w.environment_id) FROM workloads w WHERE w.id=resource
$$;
CREATE OR REPLACE FUNCTION cove_deployment_approval() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.action IN ('deploy','rollback') THEN
 SELECT EXISTS(SELECT 1 FROM workloads w JOIN deployment_policies p ON p.team_id=w.team_id AND ((p.project_id IS NULL AND p.environment_id IS NULL) OR p.project_id=w.project_id OR p.environment_id=cove_deployment_environment(w.id)) WHERE w.id=NEW.workload_id AND p.approval) INTO NEW.approval_required;
 END IF;
 RETURN NEW;
END $$;
ALTER TABLE health_deliveries ADD COLUMN IF NOT EXISTS alert_key text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS health_deliveries_incident_pending ON health_deliveries(connection_id,alert_key,created_at,id) WHERE state='pending';

ALTER TABLE workloads ADD COLUMN IF NOT EXISTS owner_user_id text;
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='workloads_owner_member' AND conrelid='workloads'::regclass) THEN
  ALTER TABLE workloads ADD CONSTRAINT workloads_owner_member FOREIGN KEY(team_id,owner_user_id) REFERENCES team_members(team_id,user_id) ON DELETE SET NULL (owner_user_id);
 END IF;
END $$;
ALTER TABLE health_deliveries ADD COLUMN IF NOT EXISTS subject_id text NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS threshold_rules (
 id text PRIMARY KEY, team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 node_id text NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 120),
 metric text NOT NULL CHECK(metric IN ('cpu','memory','disk')),
 threshold double precision NOT NULL CHECK(threshold>0 AND threshold<=100),
 confirmation_seconds integer NOT NULL CHECK(confirmation_seconds BETWEEN 60 AND 3600),
 repeat_seconds integer NOT NULL CHECK(repeat_seconds BETWEEN 300 AND 604800),
 channel_ids text[] NOT NULL CHECK(cardinality(channel_ids) BETWEEN 1 AND 32)
);
CREATE INDEX IF NOT EXISTS threshold_rules_node ON threshold_rules(node_id);

CREATE TABLE IF NOT EXISTS telemetry_settings (
 id integer PRIMARY KEY CHECK(id=1), configuration jsonb NOT NULL
);
INSERT INTO telemetry_settings(id,configuration) VALUES(1,'{"metric_rows":200000,"metric_bytes":268435456,"log_rows":200000,"log_bytes":134217728,"log_days":7,"audit_days":90,"raw_days":1,"hour_days":30,"day_days":365}') ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS metric_samples (
 id bigserial PRIMARY KEY, node_id text NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 workload_id text REFERENCES workloads(id) ON DELETE CASCADE, series text NOT NULL,
 tier text NOT NULL CHECK(tier IN ('minute','hour','day')), bucket timestamptz NOT NULL,
 sampled_at timestamptz NOT NULL, payload jsonb NOT NULL CHECK(octet_length(payload::text)<=32768)
);
CREATE UNIQUE INDEX IF NOT EXISTS metric_samples_series ON metric_samples(node_id,COALESCE(workload_id,''),series,tier,bucket);
CREATE INDEX IF NOT EXISTS metric_samples_recent ON metric_samples(sampled_at DESC,id DESC);
CREATE TABLE IF NOT EXISTS workload_log_entries (
 id text PRIMARY KEY, workload_id text NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
 container_id text NOT NULL, container text NOT NULL, timestamp timestamptz NOT NULL,
 message text NOT NULL CHECK(octet_length(message)<=8192), clipped boolean NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS workload_logs_recent ON workload_log_entries(timestamp DESC,id DESC);
CREATE INDEX IF NOT EXISTS workload_logs_resource ON workload_log_entries(workload_id,timestamp DESC,id DESC);
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS telemetry_since timestamptz;
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS telemetry_checked_at timestamptz;
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS telemetry_limited boolean NOT NULL DEFAULT false;
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS telemetry_error text NOT NULL DEFAULT '';

ALTER TABLE threshold_rules ADD COLUMN IF NOT EXISTS workload_id text REFERENCES workloads(id) ON DELETE CASCADE;
ALTER TABLE threshold_rules ADD COLUMN IF NOT EXISTS domain text NOT NULL DEFAULT '';
ALTER TABLE threshold_rules DROP CONSTRAINT IF EXISTS threshold_rules_metric_check;
ALTER TABLE threshold_rules ADD CONSTRAINT threshold_rules_metric_check CHECK(metric IN ('cpu','memory','disk','certificate','restarts','oom','crashes','uptime'));
ALTER TABLE threshold_rules DROP CONSTRAINT IF EXISTS threshold_rules_threshold_check;
ALTER TABLE threshold_rules ADD CONSTRAINT threshold_rules_threshold_check CHECK(threshold>0 AND threshold<=100000);
CREATE INDEX IF NOT EXISTS threshold_rules_workload ON threshold_rules(workload_id) WHERE workload_id IS NOT NULL;

ALTER TABLE threshold_rules ADD COLUMN IF NOT EXISTS mount_path text NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS operation_retention_runs (
 node_id text PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
 checked_at timestamptz NOT NULL,
 error text NOT NULL DEFAULT '',
 pruned integer NOT NULL DEFAULT 0,
 protected integer NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS node_costs (
 node_id text PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
 monthly_minor bigint NOT NULL CHECK(monthly_minor BETWEEN 0 AND 100000000000),
 currency text NOT NULL CHECK(currency ~ '^[A-Z]{3}$')
);

ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS metrics_token_hash bytea;
CREATE TABLE IF NOT EXISTS log_forwarders (
 id text PRIMARY KEY,team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 name text NOT NULL,enabled boolean NOT NULL,configuration bytea NOT NULL,
 last_success timestamptz,last_error text NOT NULL DEFAULT '',sent_entries bigint NOT NULL DEFAULT 0,
 dropped_entries bigint NOT NULL DEFAULT 0,UNIQUE(team_id,name)
);
CREATE TABLE IF NOT EXISTS log_forward_deliveries (
 id text PRIMARY KEY,forwarder_id text NOT NULL REFERENCES log_forwarders(id) ON DELETE CASCADE,
 payload bytea NOT NULL CHECK(octet_length(payload)<=524288),entries integer NOT NULL CHECK(entries BETWEEN 1 AND 100),
 attempts integer NOT NULL DEFAULT 0,next_attempt timestamptz NOT NULL DEFAULT now(),created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS log_forward_due ON log_forward_deliveries(next_attempt,created_at);
CREATE TABLE IF NOT EXISTS route_checks (
 id text PRIMARY KEY,workload_id text NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,domain text NOT NULL,path text NOT NULL,
 enabled boolean NOT NULL,interval_seconds integer NOT NULL CHECK(interval_seconds BETWEEN 60 AND 86400),
 timeout_seconds integer NOT NULL CHECK(timeout_seconds BETWEEN 1 AND 10),expected_min integer NOT NULL,expected_max integer NOT NULL,
 public_label text NOT NULL DEFAULT '',next_check_at timestamptz NOT NULL DEFAULT now(),checked_at timestamptz,
 last_up boolean,last_status integer,latency_ms double precision,last_error text NOT NULL DEFAULT '',
 consecutive_failures integer NOT NULL DEFAULT 0,version integer NOT NULL DEFAULT 1,UNIQUE(workload_id,domain)
);
CREATE INDEX IF NOT EXISTS route_checks_due ON route_checks(next_check_at) WHERE enabled;
CREATE TABLE IF NOT EXISTS route_check_results (
 id bigserial PRIMARY KEY,check_id text NOT NULL REFERENCES route_checks(id) ON DELETE CASCADE,
 checked_at timestamptz NOT NULL,up boolean NOT NULL,status integer NOT NULL,latency_ms double precision NOT NULL
);
CREATE INDEX IF NOT EXISTS route_check_history ON route_check_results(check_id,checked_at DESC);
CREATE INDEX IF NOT EXISTS route_check_result_age ON route_check_results(checked_at);
CREATE TABLE IF NOT EXISTS project_status_pages (
 project_id text PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,enabled boolean NOT NULL DEFAULT false,
 title text NOT NULL,slug text NOT NULL UNIQUE CHECK(length(slug)=64)
);
CREATE TABLE IF NOT EXISTS traffic_collection (
 node_id text PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,cursor_at timestamptz,checked_at timestamptz,
 available boolean NOT NULL DEFAULT false,limited boolean NOT NULL DEFAULT false,last_error text NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS route_traffic (
 id text PRIMARY KEY,workload_id text NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
 node_id text NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,domain text NOT NULL,timestamp timestamptz NOT NULL,
 status integer NOT NULL,bytes bigint NOT NULL,duration_ms double precision NOT NULL,client_ip text NOT NULL
);
CREATE INDEX IF NOT EXISTS route_traffic_recent ON route_traffic(timestamp DESC,id DESC);
CREATE INDEX IF NOT EXISTS route_traffic_resource ON route_traffic(workload_id,timestamp DESC);

-- Backup transfer bounds.
ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS backup_transfer_bytes bigint NOT NULL DEFAULT 1073741824 CHECK (backup_transfer_bytes BETWEEN 1048576 AND 68719476736);

-- Host-driven panel backups and recovery.
CREATE TABLE IF NOT EXISTS panel_recovery (
 id integer PRIMARY KEY CHECK(id=1),configuration bytea,
 agent_id text NOT NULL DEFAULT '',checked_at timestamptz,last_scheduled_at timestamptz,
 updates_paused boolean NOT NULL DEFAULT false,last_error text NOT NULL DEFAULT '',pending_recovery boolean NOT NULL DEFAULT false,pending_remote_job text NOT NULL DEFAULT '',
 snapshots jsonb NOT NULL DEFAULT '[]' CHECK(jsonb_typeof(snapshots)='array' AND jsonb_array_length(snapshots)<=102)
);
INSERT INTO panel_recovery(id) VALUES(1) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS panel_recovery_jobs (
 id text PRIMARY KEY,actor_id text NOT NULL,action text NOT NULL CHECK(action IN ('backup','restore','resume_updates')),
 snapshot_id text NOT NULL DEFAULT '',state text NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','running','succeeded','failed')),
 requested_at timestamptz NOT NULL DEFAULT now(),finished_at timestamptz,last_error text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS one_panel_recovery_job ON panel_recovery_jobs((true)) WHERE state IN ('queued','running');
CREATE INDEX IF NOT EXISTS panel_recovery_history ON panel_recovery_jobs(requested_at DESC,id DESC);

-- Metrics service capabilities are revalidated against their issuing administrator.
ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS metrics_token_creator_id text REFERENCES users(id) ON DELETE SET NULL;

-- Route safe audit outcomes through the existing channel delivery queue.
CREATE OR REPLACE FUNCTION cove_queue_channel_event(event_name text, event_message text, source_team text, resource text, delivery text, affected_user text DEFAULT '') RETURNS void LANGUAGE sql AS $$
 INSERT INTO health_deliveries(id,team_id,connection_id,event,message,alert_key,subject_id)
 SELECT delivery||':'||c.id,c.team_id,c.id,event_name,event_message,delivery,resource
 FROM infrastructure_connections c WHERE c.kind='notification' AND
 ((source_team IS NOT NULL AND c.team_id=source_team) OR
 (source_team IS NULL AND affected_user<>'' AND EXISTS(SELECT 1 FROM team_members m WHERE m.team_id=c.team_id AND m.user_id=affected_user)) OR
 (source_team IS NULL AND affected_user='' AND EXISTS(SELECT 1 FROM team_members m JOIN users u ON u.id=m.user_id WHERE m.team_id=c.team_id AND u.is_admin AND NOT u.disabled)))
 ON CONFLICT(id) DO NOTHING;
$$;
CREATE OR REPLACE FUNCTION cove_notify_audit_outcome() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE event_name text; event_message text; affected text:=''; resource text:=''; source_team text:=NEW.team_id;
BEGIN
 IF NEW.action='authentication lockout' THEN event_name='security.lockout'; event_message='An account authentication lockout occurred.'; affected=NEW.actor;
 ELSIF NEW.action='account new device' THEN event_name='security.new_device'; event_message='An account signed in from a new browser. Review account login history.'; affected=NEW.actor;
 ELSIF NEW.action='DELETE /api/users/{id}/second-factor' AND NEW.status<300 THEN event_name='security.second_factor_reset'; event_message='An administrator reset an account second factor.'; affected=NEW.subject; source_team=NULL;
 ELSIF NEW.action='emergency deployment override' AND NEW.status<300 THEN event_name='deployment.freeze_override'; event_message='An administrator overrode deployment policy. Review the audit log.';
 ELSIF NEW.actor='deploy hook' AND NEW.action='accepted delivery' THEN event_name='deploy_hook.accepted'; event_message='A deploy hook delivery was accepted. Deployment policy still applies.';
 ELSIF NEW.actor='deploy hook' AND NEW.status>=400 THEN event_name='deploy_hook.failed'; event_message='A deploy hook delivery was refused. Review its audit event.';
 ELSIF NEW.action='GitOps diff staged' THEN event_name='gitops.drift'; event_message='GitOps compared its source with saved configuration. Review its configuration diff.';
 ELSIF NEW.action='configuration applied' AND NEW.status<300 THEN
  IF EXISTS(SELECT 1 FROM configuration_gitops g WHERE g.project_id=NEW.subject AND g.last_draft_id=NEW.details->>'draft_id') THEN event_name='gitops.applied'; ELSE event_name='configuration.applied'; END IF;
  event_message='Reviewed project configuration was saved. This did not deploy resources.';
 ELSIF NEW.action LIKE 'POST /api/projects/{id}/imports%' THEN
  IF NEW.status<300 THEN event_name='import.reviewed'; event_message='An importer prepared a review. The source system was not changed.'; ELSE event_name='import.failed'; event_message='An importer request was refused. Review the audit log.'; END IF;
 ELSIF NEW.actor='cpu-scaler' AND NEW.action='scale service replicas' THEN event_name='autoscale.queued'; event_message='CPU observations queued a replica change. Deployment admission still applies.';
 ELSIF NEW.action='idle state observed' AND NEW.details->>'state'='asleep' THEN event_name='resource.asleep'; event_message='The request-driven resource was observed asleep. Availability checks are deferred.';
 ELSIF NEW.action='idle state observed' AND NEW.details->>'state'='awake' THEN event_name='resource.awake'; event_message='The request-driven resource was observed awake.';
 ELSIF NEW.action='resource.adopt' THEN event_name='adoption.succeeded'; event_message='A reviewed resource was adopted without restarting containers.';
 ELSIF NEW.action LIKE 'POST /api/nodes/{id}/adoptions%' AND NEW.status>=400 THEN event_name='adoption.failed'; event_message='A Compose adoption request did not finish. Reload its review before retrying.';
 ELSE RETURN NEW;
 END IF;
 SELECT id INTO resource FROM workloads WHERE id=NEW.subject AND (NEW.team_id IS NULL OR team_id=NEW.team_id);
 IF source_team IS NULL AND resource IS NOT NULL THEN SELECT team_id INTO source_team FROM workloads WHERE id=resource; END IF;
 PERFORM cove_queue_channel_event(event_name,event_message,source_team,COALESCE(resource,''),'audit:'||NEW.id,affected);
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS cove_audit_notifications ON audit_events;
CREATE TRIGGER cove_audit_notifications AFTER INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION cove_notify_audit_outcome();
CREATE OR REPLACE FUNCTION cove_audit_lockout() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.locked_until>now() AND (TG_OP='INSERT' OR OLD.locked_until<=now()) THEN
  INSERT INTO audit_events(actor,action,subject,status) VALUES(CASE WHEN NEW.key LIKE 'account:%' THEN substring(NEW.key from 9) ELSE '' END,'authentication lockout','',429);
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS cove_lockout_notification ON authentication_failures;
CREATE TRIGGER cove_lockout_notification AFTER INSERT OR UPDATE ON authentication_failures FOR EACH ROW EXECUTE FUNCTION cove_audit_lockout();
-- Deploy track: per-job calendar timezone. Existing schedules retain UTC semantics.
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS job_timezone text NOT NULL DEFAULT 'UTC';

-- Deploy track: bounded hourly image digest checks use the existing durable operation queue.
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS next_image_check_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX IF NOT EXISTS due_image_checks ON workloads(next_image_check_at) WHERE kind='service';
-- Metadata polling must not generate generic deployment notifications every hour.
DROP TRIGGER IF EXISTS cove_operation_notification ON operations;
CREATE TRIGGER cove_operation_notification AFTER UPDATE OF state ON operations FOR EACH ROW
 WHEN (OLD.state IS DISTINCT FROM NEW.state AND NEW.action <> 'image_check' AND NEW.state IN ('succeeded','failed','interrupted','cancelled')) EXECUTE FUNCTION cove_notify_operation();

-- Deploy track: scheduled runs coalesce at most one tick and bound execution.
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS job_overlap_policy text NOT NULL DEFAULT 'skip' CHECK (job_overlap_policy IN ('skip','queue_one','replace'));
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS job_timeout_seconds integer NOT NULL DEFAULT 1800 CHECK (job_timeout_seconds BETWEEN 5 AND 86400);
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS job_pending_at timestamptz;

-- Deploy track: health restarts share the durable operation queue and workload lock.
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS restart_unhealthy boolean NOT NULL DEFAULT false;
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS next_unhealthy_check_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS last_unhealthy_restart_at timestamptz;
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS unhealthy_restart_operation_id text REFERENCES operations(id) ON DELETE SET NULL;
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS unhealthy_restart_error text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS unhealthy_restart_attempts ON operations(workload_id,created_at) WHERE action='restart_unhealthy';

-- Deploy track: desired environment suspension and worker-confirmed resource checkpoints.
ALTER TABLE environments ADD COLUMN IF NOT EXISTS paused boolean NOT NULL DEFAULT false;
CREATE TABLE IF NOT EXISTS environment_suspensions (
 workload_id text PRIMARY KEY REFERENCES workloads(id) ON DELETE CASCADE,
 environment_id text NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
 suspended boolean NOT NULL DEFAULT false,
 resume_wanted boolean NOT NULL DEFAULT false,
 operation_id text REFERENCES operations(id),
 action text NOT NULL DEFAULT '' CHECK (action IN ('','suspend','resume')),
 error text NOT NULL DEFAULT ''
);
CREATE OR REPLACE FUNCTION cove_cancel_paused_operation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE held boolean;
BEGIN
 IF NEW.action IN ('deploy','rollback','start','restart','restart_unhealthy','rollout_promote','restore','drill') THEN
  SELECT e.paused INTO held FROM environments e JOIN workloads w ON w.environment_id=e.id WHERE w.id=NEW.workload_id FOR SHARE OF e;
  IF held THEN NEW.cancel_requested := true; END IF;
 END IF;
 RETURN NEW;
END
$$;
DROP TRIGGER IF EXISTS cove_paused_operation ON operations;
CREATE TRIGGER cove_paused_operation BEFORE INSERT ON operations FOR EACH ROW EXECUTE FUNCTION cove_cancel_paused_operation();

-- Deploy track: desired idle sleep policy is committed only after worker route confirmation.
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS idle_policy jsonb NOT NULL DEFAULT '{"enabled":false,"after_seconds":300,"request_driven":false}'::jsonb;

-- CPU scaling samples and attempt cooldown survive control-plane restarts.
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS next_cpu_check_at timestamptz;
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS cpu_samples jsonb NOT NULL DEFAULT '[]';
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS last_cpu_scale_at timestamptz;
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS cpu_scale_operation_id text NOT NULL DEFAULT '';
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS cpu_scale_status text NOT NULL DEFAULT 'disabled';
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS cpu_scale_error text NOT NULL DEFAULT '';

-- Cross-track observed runtime states and durable rollout notification outcomes.
ALTER TABLE workloads ADD COLUMN IF NOT EXISTS observed_idle_state text NOT NULL DEFAULT 'awake';
CREATE OR REPLACE FUNCTION cove_notify_deployment_outcome() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE event_name text; event_message text; source_team text;
BEGIN
 IF NEW.state NOT IN ('succeeded','failed','interrupted','cancelled') OR (OLD.state=NEW.state AND OLD.result IS NOT DISTINCT FROM NEW.result) THEN RETURN NEW; END IF;
 IF EXISTS(SELECT 1 FROM workloads WHERE id=NEW.workload_id AND cpu_scale_operation_id=NEW.id) THEN
  IF NEW.state='succeeded' THEN event_name='autoscale.succeeded'; event_message='The observed CPU replica change completed.'; ELSE event_name='autoscale.failed'; event_message='The CPU replica change did not complete. Review its operation.'; END IF;
 ELSIF NEW.action='rollout_abort' AND NEW.state='succeeded' THEN event_name='rollout.aborted'; event_message='The staged rollout was aborted and retained traffic restored.';
 ELSIF NEW.action='rollout_promote' AND NEW.state='succeeded' THEN event_name='rollout.promoted'; event_message='The rollout became active.';
 ELSIF NEW.action IN ('deploy','rollback') AND jsonb_typeof(NEW.result->'rollout')='object' THEN
  IF NEW.state<>'succeeded' THEN event_name='rollout.failed'; event_message='The rollout did not complete. Review its operation and retained state.';
  ELSIF NEW.result->'rollout'->>'phase'='waiting' THEN event_name='rollout.staged'; event_message='A healthy candidate is staged for an explicit rollout decision.';
  ELSIF NEW.result->'rollout'->>'phase'='aborted' THEN event_name='rollout.aborted'; event_message='The rollout was aborted and retained traffic restored.';
  ELSE event_name='rollout.promoted'; event_message='The rollout became active.'; END IF;
 ELSIF NEW.action IN ('rollout_promote','rollout_abort') THEN event_name='rollout.failed'; event_message='The rollout decision did not complete. Review its operation.';
 ELSE RETURN NEW;
 END IF;
 SELECT team_id INTO source_team FROM workloads WHERE id=NEW.workload_id;
 PERFORM cove_queue_channel_event(event_name,event_message,source_team,NEW.workload_id,'operation:'||NEW.id||':'||event_name);
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS cove_deployment_notifications ON operations;
CREATE TRIGGER cove_deployment_notifications AFTER UPDATE OF state,result ON operations FOR EACH ROW EXECUTE FUNCTION cove_notify_deployment_outcome();

-- Rollout promotion changes live release traffic and inherits preview-aware approval policies.
CREATE OR REPLACE FUNCTION cove_deployment_approval() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.action IN ('deploy','rollback','rollout_promote') THEN
 SELECT EXISTS(SELECT 1 FROM workloads w JOIN deployment_policies p ON p.team_id=w.team_id AND ((p.project_id IS NULL AND p.environment_id IS NULL) OR p.project_id=w.project_id OR p.environment_id=cove_deployment_environment(w.id)) WHERE w.id=NEW.workload_id AND p.approval) INTO NEW.approval_required;
 END IF;
 RETURN NEW;
END $$;

-- A staged candidate is not the deployed declaration until an explicit promotion commits it.
CREATE OR REPLACE VIEW cove_committed_deployments AS
 SELECT decision.workload_id,decision.id AS operation_id,declaration.id AS declaration_id,decision.updated_at,COALESCE(declaration.result->>'revision','') AS revision
 FROM operations decision JOIN operations declaration ON declaration.workload_id=decision.workload_id AND declaration.id=CASE WHEN decision.action='rollout_promote' THEN decision.result->'rollout'->>'release' ELSE decision.id END
 WHERE decision.state='succeeded' AND decision.action IN ('deploy','rollback','adopt','rollout_promote')
 AND COALESCE(decision.result->'rollout'->>'phase','') NOT IN ('waiting','aborted')
 AND (decision.action<>'rollout_promote' OR decision.result->'rollout'->>'phase'='completed');


-- Provider feedback follows the traffic decision rather than successful staging.
CREATE INDEX IF NOT EXISTS operations_rollout_decisions ON operations(workload_id,(result->'rollout'->>'release'),updated_at DESC,id DESC)
 WHERE state='succeeded' AND action IN ('rollout_promote','rollout_abort','stop');
CREATE OR REPLACE VIEW cove_deployment_feedback_states AS
 SELECT source.id,CASE WHEN source.state='succeeded' AND source.result->'rollout'->>'phase'='waiting'
  THEN CASE decision.phase WHEN 'completed' THEN 'succeeded' WHEN 'aborted' THEN 'cancelled' ELSE 'running' END
  ELSE source.state END AS state
 FROM operations source LEFT JOIN LATERAL (
  SELECT outcome.result->'rollout'->>'phase' AS phase FROM operations outcome
  WHERE source.state='succeeded' AND source.result->'rollout'->>'phase'='waiting'
   AND outcome.workload_id=source.workload_id AND outcome.state='succeeded'
   AND outcome.action IN ('rollout_promote','rollout_abort','stop')
   AND outcome.result->'rollout'->>'release'=source.id
   AND outcome.result->'rollout'->>'phase' IN ('completed','aborted')
  ORDER BY outcome.updated_at DESC,outcome.id DESC LIMIT 1
 ) decision ON true WHERE source.action='deploy';


CREATE TABLE IF NOT EXISTS database_maintenance (
 id text PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE, workload_id text NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
 source_id text REFERENCES workloads(id) ON DELETE SET NULL, node_id text NOT NULL REFERENCES nodes(id),
 kind text NOT NULL CHECK(kind IN ('rotate','extension','upgrade')),
 configuration bytea NOT NULL,
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','failed','succeeded','discarded')),
 last_error text NOT NULL DEFAULT '', retry_requested boolean NOT NULL DEFAULT false, created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS database_maintenance_active ON database_maintenance(workload_id) WHERE state IN ('pending','failed');
ALTER TABLE database_maintenance ADD COLUMN IF NOT EXISTS cutover_at timestamptz;
ALTER TABLE database_maintenance ADD COLUMN IF NOT EXISTS cutover_by text NOT NULL DEFAULT '';
CREATE OR REPLACE FUNCTION cove_database_maintenance_lock() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE resource text;
BEGIN
 resource:=OLD.id;
 PERFORM 1 FROM workloads WHERE id=resource FOR UPDATE;
 IF EXISTS(SELECT 1 FROM database_maintenance WHERE (state IN ('pending','failed') AND workload_id=resource) OR (state='pending' AND source_id=resource)) OR EXISTS(SELECT 1 FROM object_buckets WHERE state='pending' AND (workload_id=resource OR storage_resource_id=resource)) THEN
  RAISE EXCEPTION 'database maintenance requires confirmation or retry before changing this resource';
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS database_maintenance_workloads ON workloads;
CREATE TRIGGER database_maintenance_workloads BEFORE UPDATE OF configuration,node_id,project_id,environment_id OR DELETE ON workloads FOR EACH ROW EXECUTE FUNCTION cove_database_maintenance_lock();

ALTER TABLE database_access ADD COLUMN IF NOT EXISTS tool text NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS object_buckets (
 id text PRIMARY KEY, workload_id text NOT NULL REFERENCES workloads(id),
 storage_resource_id text NOT NULL REFERENCES workloads(id),
 storage_id text REFERENCES infrastructure_connections(id),
 configuration bytea NOT NULL, state text NOT NULL DEFAULT 'pending',
 created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(workload_id)
);

-- Resource-scoped immutable TLS versions keep private keys outside configuration and history.
CREATE TABLE IF NOT EXISTS route_certificates (
 id text PRIMARY KEY,
 workload_id text NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
 name text NOT NULL,
 material bytea NOT NULL,
 metadata jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS route_certificates_workload_idx ON route_certificates(workload_id);

-- Issuer ownership and pending private requests survive outages and prevent premature resource deletion.
ALTER TABLE route_certificates ADD COLUMN IF NOT EXISTS source text NOT NULL DEFAULT 'uploaded';
CREATE TABLE IF NOT EXISTS route_certificate_issuers (
 id text PRIMARY KEY,
 workload_id text NOT NULL REFERENCES workloads(id),
 zone_id text NOT NULL REFERENCES cloudflare_zones(zone_id),
 name text NOT NULL,
 provider text NOT NULL CHECK (provider IN ('cloudflare_origin')),
 domains text[] NOT NULL,
 validity_days integer NOT NULL,
 auto_renew boolean NOT NULL,
 phase text NOT NULL DEFAULT 'requesting',
 state bytea NOT NULL,
 certificate_id text REFERENCES route_certificates(id),
 operation_id text NOT NULL DEFAULT '',
 last_error text NOT NULL DEFAULT '',
 next_check_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS route_certificate_issuers_due ON route_certificate_issuers(next_check_at) WHERE next_check_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS route_certificate_issuers_workload ON route_certificate_issuers(workload_id);
