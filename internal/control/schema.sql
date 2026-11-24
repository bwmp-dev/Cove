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
CREATE TABLE IF NOT EXISTS image_scanning_settings (
 team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
 node_id text NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 enabled boolean NOT NULL DEFAULT false, notify boolean NOT NULL DEFAULT false,
 PRIMARY KEY(team_id,node_id)
);
CREATE OR REPLACE FUNCTION cove_notify_operation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 INSERT INTO notification_deliveries(operation_id,connection_id)
  SELECT NEW.id,c.id FROM infrastructure_connections c JOIN workloads w ON w.team_id=c.team_id
  WHERE w.id=NEW.workload_id AND c.kind='notification' AND c.created_at<=NEW.created_at
  AND (NEW.action<>'image_scan' OR EXISTS(SELECT 1 FROM image_scanning_settings ss WHERE ss.team_id=w.team_id AND ss.node_id=w.node_id AND ss.notify)) ON CONFLICT DO NOTHING;
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

-- DNS-01 request identity and cleanup ownership survive issuer/resource removal.
ALTER TABLE route_certificate_issuers DROP CONSTRAINT IF EXISTS route_certificate_issuers_provider_check;
ALTER TABLE route_certificate_issuers ADD CONSTRAINT route_certificate_issuers_provider_check CHECK (provider IN ('cloudflare_origin','acme_dns'));
ALTER TABLE route_certificate_issuers ADD COLUMN IF NOT EXISTS acme_state bytea;
CREATE TABLE IF NOT EXISTS route_certificate_acme_accounts (
 directory text PRIMARY KEY,
 private_key bytea NOT NULL,
 account_uri text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS route_certificate_dns_intents (
 id text PRIMARY KEY,
 issuer_id text REFERENCES route_certificate_issuers(id) ON DELETE SET NULL,
 workload_id text REFERENCES workloads(id) ON DELETE SET NULL,
 zone_id text NOT NULL REFERENCES cloudflare_zones(zone_id),
 cycle_id text NOT NULL,
 state bytea NOT NULL,
 cleanup_requested boolean NOT NULL DEFAULT false,
 phase text NOT NULL DEFAULT 'pending',
 last_error text NOT NULL DEFAULT '',
 next_check_at timestamptz DEFAULT now(),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS route_certificate_dns_intents_due ON route_certificate_dns_intents(next_check_at) WHERE phase<>'cleaned';
CREATE INDEX IF NOT EXISTS route_certificate_dns_intents_zone ON route_certificate_dns_intents(zone_id);
CREATE INDEX IF NOT EXISTS route_certificate_dns_intents_issuer ON route_certificate_dns_intents(issuer_id);

CREATE OR REPLACE FUNCTION cove_queue_certificate_dns_cleanup() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (OLD.issuer_id IS NOT NULL AND NEW.issuer_id IS NULL) OR (OLD.workload_id IS NOT NULL AND NEW.workload_id IS NULL) THEN
  NEW.cleanup_requested := true;
  NEW.updated_at := now();
  IF NEW.phase <> 'cleaned' THEN
   NEW.phase := 'cleanup';
   NEW.next_check_at := now();
  END IF;
 END IF;
 RETURN NEW;
END
$$;
DROP TRIGGER IF EXISTS cove_certificate_dns_orphan ON route_certificate_dns_intents;
CREATE TRIGGER cove_certificate_dns_orphan BEFORE UPDATE OF issuer_id,workload_id ON route_certificate_dns_intents FOR EACH ROW EXECUTE FUNCTION cove_queue_certificate_dns_cleanup();

CREATE TABLE IF NOT EXISTS node_cloudflare_tunnels (
 node_id text PRIMARY KEY REFERENCES nodes(id) ON DELETE RESTRICT,
 id text NOT NULL UNIQUE,
 zone_id text NOT NULL REFERENCES cloudflare_zones(zone_id) ON DELETE RESTRICT,
 connection_id text NOT NULL REFERENCES cloudflare_accounts(id) ON DELETE RESTRICT,
 owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
 hostnames text[] NOT NULL,
 image text NOT NULL,
 phase text NOT NULL DEFAULT 'pending',
 state bytea NOT NULL,
 cleanup_requested boolean NOT NULL DEFAULT false,
 last_error text NOT NULL DEFAULT '',
 next_check_at timestamptz DEFAULT now(),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS registry_webhooks (
 workload_id text PRIMARY KEY REFERENCES workloads(id) ON DELETE CASCADE,
 id text NOT NULL UNIQUE,
 registry_id text NOT NULL REFERENCES infrastructure_connections(id) ON DELETE RESTRICT,
 registry_server text NOT NULL,
	provider text NOT NULL CHECK(provider IN ('distribution','docker_hub','ghcr')),
 enabled boolean NOT NULL,
 filters jsonb NOT NULL CHECK(jsonb_typeof(filters)='array' AND jsonb_array_length(filters) BETWEEN 1 AND 32),
 secret bytea NOT NULL,
 last_received_at timestamptz,
 last_matched_at timestamptz,
	pending_at timestamptz,
	next_dispatch_at timestamptz NOT NULL DEFAULT now(),
	last_operation_id text REFERENCES operations(id) ON DELETE SET NULL,
	last_error text NOT NULL DEFAULT '',
	last_ignored_reason text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS registry_webhook_deliveries (
 webhook_id text NOT NULL REFERENCES registry_webhooks(id) ON DELETE CASCADE,
 event_key text NOT NULL CHECK(event_key ~ '^[a-f0-9]{64}$'),
	source_key text NOT NULL CHECK(source_key ~ '^[a-f0-9]{64}$'),
 image_key text NOT NULL CHECK(image_key ~ '^[a-f0-9]{64}$'),
 received_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(webhook_id,event_key),
	UNIQUE(webhook_id,source_key)
);
CREATE INDEX IF NOT EXISTS registry_webhook_deliveries_age ON registry_webhook_deliveries(webhook_id,received_at);

CREATE TABLE IF NOT EXISTS image_update_policies (
 workload_id text PRIMARY KEY REFERENCES workloads(id) ON DELETE CASCADE,
 policy jsonb NOT NULL, generation text NOT NULL,
 last_check_id text REFERENCES operations(id) ON DELETE SET NULL,
 last_operation_id text REFERENCES operations(id) ON DELETE SET NULL,
 consumed_candidate text NOT NULL DEFAULT '', last_error text NOT NULL DEFAULT '',
 updated_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE image_update_policies ADD COLUMN IF NOT EXISTS next_dispatch_at timestamptz NOT NULL DEFAULT now();
CREATE TABLE IF NOT EXISTS image_update_deployments (
 operation_id text PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
 window_start text NOT NULL, window_end text NOT NULL
);

CREATE TABLE IF NOT EXISTS image_update_events (
 operation_id text PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
 summary text NOT NULL CHECK(length(summary)<=32768)
);

ALTER TABLE image_update_policies ADD COLUMN IF NOT EXISTS processed_check_id text REFERENCES operations(id) ON DELETE SET NULL;

CREATE TABLE IF NOT EXISTS managed_registries (
 id text PRIMARY KEY REFERENCES workloads(id) ON DELETE RESTRICT,
 team_id text NOT NULL REFERENCES teams(id) ON DELETE RESTRICT,
 hostname text NOT NULL,
 credentials bytea NOT NULL,
 retention jsonb NOT NULL,
 state text NOT NULL DEFAULT 'provisioning' CHECK(state IN ('provisioning','ready','maintenance','removing','failed','removed')),
 previous_state text NOT NULL DEFAULT '',
 connection_id text UNIQUE REFERENCES infrastructure_connections(id) ON DELETE RESTRICT,
 operation_id text REFERENCES operations(id) ON DELETE RESTRICT,
 last_error text NOT NULL DEFAULT '', data_retained boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS one_live_managed_registry_per_team ON managed_registries(team_id) WHERE state<>'removed';
CREATE UNIQUE INDEX IF NOT EXISTS managed_registry_hostname ON managed_registries(hostname) WHERE state<>'removed';
CREATE OR REPLACE FUNCTION cove_managed_registry_admission() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE registry_state text;
BEGIN
 IF NEW.state IN ('queued','running') AND NEW.action IN ('build','deploy','rollback') THEN
  FOR registry_state IN SELECT m.state FROM managed_registries m JOIN workloads w ON w.team_id=m.team_id WHERE w.id=NEW.workload_id AND (m.state<>'removed' OR m.data_retained) ORDER BY m.id FOR SHARE OF m LOOP
   IF registry_state IN ('maintenance','removing') THEN
    RAISE EXCEPTION 'managed registry maintenance holds team publishers' USING ERRCODE='23514',CONSTRAINT='cove_managed_registry_busy';
   END IF;
  END LOOP;
  SELECT m.state INTO registry_state FROM managed_registries m JOIN workload_connections c ON c.connection_id=m.connection_id WHERE c.workload_id=NEW.workload_id AND c.kind='registry' FOR SHARE OF m;
  IF registry_state IS NOT NULL AND registry_state<>'ready' THEN
   RAISE EXCEPTION 'managed registry is not ready for publishing or deployment' USING ERRCODE='23514',CONSTRAINT='cove_managed_registry_busy';
  END IF;
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS cove_managed_registry_admission ON operations;
CREATE TRIGGER cove_managed_registry_admission BEFORE INSERT ON operations FOR EACH ROW EXECUTE FUNCTION cove_managed_registry_admission();
CREATE OR REPLACE FUNCTION cove_managed_registry_binding() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE registry_state text;
BEGIN
 SELECT state INTO registry_state FROM managed_registries WHERE connection_id=NEW.connection_id FOR SHARE;
 IF registry_state IS NOT NULL AND registry_state<>'ready' THEN
  RAISE EXCEPTION 'managed registry is not ready for new connections' USING ERRCODE='23514',CONSTRAINT='cove_managed_registry_busy';
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS cove_managed_registry_binding ON workload_connections;
CREATE TRIGGER cove_managed_registry_binding BEFORE INSERT OR UPDATE OF connection_id ON workload_connections FOR EACH ROW EXECUTE FUNCTION cove_managed_registry_binding();

CREATE TABLE IF NOT EXISTS workload_build_nodes (
 workload_id text PRIMARY KEY REFERENCES workloads(id) ON DELETE CASCADE,
 node_id text NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS workload_build_recipes (
 workload_id text PRIMARY KEY REFERENCES workloads(id) ON DELETE CASCADE,
 configuration bytea NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE operations ADD COLUMN IF NOT EXISTS execution_node_id text REFERENCES nodes(id) ON DELETE RESTRICT;
ALTER TABLE operations ADD COLUMN IF NOT EXISTS build_prepared boolean NOT NULL DEFAULT false;
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='operations' AND column_name='build_legacy') THEN
  ALTER TABLE operations ADD COLUMN build_legacy boolean NOT NULL DEFAULT false;
  UPDATE operations SET build_legacy=true WHERE action='build' AND state IN ('queued','running');
 END IF;
END $$;
UPDATE operations o SET execution_node_id=w.node_id FROM workloads w WHERE o.workload_id=w.id AND o.action='build' AND o.state IN ('queued','running') AND o.execution_node_id IS NULL;
CREATE OR REPLACE FUNCTION cove_build_execution_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.execution_node_id IS NOT NULL AND NEW.execution_node_id IS DISTINCT FROM OLD.execution_node_id THEN
  RAISE EXCEPTION 'build execution node is immutable' USING ERRCODE='23514';
 END IF;
 IF OLD.action='build' AND OLD.build_prepared AND (NOT NEW.build_prepared OR NEW.payload IS DISTINCT FROM OLD.payload) THEN
  RAISE EXCEPTION 'prepared build inputs are immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS cove_build_execution_identity ON operations;
CREATE TRIGGER cove_build_execution_identity BEFORE UPDATE OF execution_node_id,payload,build_prepared ON operations FOR EACH ROW EXECUTE FUNCTION cove_build_execution_identity();
