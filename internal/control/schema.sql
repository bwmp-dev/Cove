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
INSERT INTO telemetry_settings(id,configuration) VALUES(1,'{"metric_rows":200000,"metric_bytes":268435456,"log_rows":200000,"log_bytes":134217728,"log_days":7,"audit_rows":100000,"audit_days":90,"raw_days":1,"hour_days":30,"day_days":365}') ON CONFLICT DO NOTHING;
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

-- Encrypted PostgreSQL PITR policies; collection is bounded to 256 resources globally.
CREATE TABLE IF NOT EXISTS workload_pitr (
 workload_id text PRIMARY KEY REFERENCES workloads(id) ON DELETE CASCADE,
 configuration bytea NOT NULL,enabled boolean NOT NULL,next_backup_at timestamptz NOT NULL DEFAULT now(),
 operation_id text REFERENCES operations(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS workload_pitr_due ON workload_pitr(next_backup_at) WHERE enabled;

-- Streamed resource file transfer bounds; the text editor remains independently bounded.
ALTER TABLE control_settings ADD COLUMN IF NOT EXISTS file_transfer_bytes bigint NOT NULL DEFAULT 67108864 CHECK(file_transfer_bytes BETWEEN 1048576 AND 1073741824);

-- Separately pinned forwarding identities never relax a worker's restricted RPC key.
CREATE TABLE IF NOT EXISTS ssh_bastions (
 id text PRIMARY KEY,name text NOT NULL,address text NOT NULL,port integer NOT NULL CHECK(port BETWEEN 1 AND 65535),
 login text NOT NULL,host_key text NOT NULL,public_key text NOT NULL,private_key bytea NOT NULL
);
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS bastion_id text REFERENCES ssh_bastions(id) ON DELETE RESTRICT;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS host_key_error text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS nodes_bastion ON nodes(bastion_id) WHERE bastion_id IS NOT NULL;

-- One resumable SSH key change and last outcome per node, with sealed key material removed at completion.
CREATE TABLE IF NOT EXISTS node_identity_rotations (
 node_id text PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,id text NOT NULL,
 phase text NOT NULL CHECK(phase IN ('queued','staged','switched','aborting','succeeded','cancelled')),
 configuration bytea,last_error text NOT NULL DEFAULT '',next_attempt_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now()
);

-- Instance-admin cloud credentials and bounded resumable server creation.
CREATE TABLE IF NOT EXISTS node_providers (
 id text PRIMARY KEY,
 kind text NOT NULL CHECK(kind IN ('hetzner','digitalocean','vultr')),
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 120),
 secret bytea NOT NULL CHECK(octet_length(secret) BETWEEN 16 AND 8192),
 created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS cloud_provider_id text REFERENCES node_providers(id) ON DELETE SET NULL;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS cloud_kind text NOT NULL DEFAULT '' CHECK(cloud_kind IN ('','hetzner','digitalocean','vultr'));
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS cloud_server_id text NOT NULL DEFAULT '' CHECK(length(cloud_server_id)<=128);
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS cloud_server_name text NOT NULL DEFAULT '' CHECK(length(cloud_server_name)<=128);
CREATE TABLE IF NOT EXISTS node_provisions (
 id text PRIMARY KEY,
 provider_id text REFERENCES node_providers(id) ON DELETE SET NULL,
 provider_kind text NOT NULL CHECK(provider_kind IN ('hetzner','digitalocean','vultr')),
 provider_name text NOT NULL CHECK(length(provider_name) BETWEEN 1 AND 120),
 team_id text NOT NULL REFERENCES teams(id),
 node_id text REFERENCES nodes(id) ON DELETE SET NULL,
 cloud_id text NOT NULL DEFAULT '' CHECK(length(cloud_id)<=128),
 cloud_name text NOT NULL CHECK(length(cloud_name) BETWEEN 1 AND 128),
 phase text NOT NULL DEFAULT 'queued' CHECK(phase IN ('queued','creating','server_pending','enrolling','succeeded','failed','uncertain','cancelled')),
 configuration bytea NOT NULL CHECK(octet_length(configuration)<=16384),
 last_error text NOT NULL DEFAULT '' CHECK(length(last_error)<=1024),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 next_check_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE node_provisions ADD COLUMN IF NOT EXISTS deadline timestamptz NOT NULL DEFAULT now()+interval '20 minutes';
CREATE INDEX IF NOT EXISTS node_provision_active_idx ON node_provisions(next_check_at) WHERE phase IN ('queued','creating','server_pending','enrolling');

-- Defaults are one bounded CPU/memory pair per existing team or environment.
ALTER TABLE teams ADD COLUMN IF NOT EXISTS resource_default_cpu double precision NOT NULL DEFAULT 0 CHECK(resource_default_cpu=0 OR resource_default_cpu BETWEEN 0.01 AND 1024);
ALTER TABLE teams ADD COLUMN IF NOT EXISTS resource_default_memory_mb bigint NOT NULL DEFAULT 0 CHECK(resource_default_memory_mb=0 OR resource_default_memory_mb BETWEEN 6 AND 16777216);
ALTER TABLE environments ADD COLUMN IF NOT EXISTS resource_default_cpu double precision NOT NULL DEFAULT 0 CHECK(resource_default_cpu=0 OR resource_default_cpu BETWEEN 0.01 AND 1024);
ALTER TABLE environments ADD COLUMN IF NOT EXISTS resource_default_memory_mb bigint NOT NULL DEFAULT 0 CHECK(resource_default_memory_mb=0 OR resource_default_memory_mb BETWEEN 6 AND 16777216);
