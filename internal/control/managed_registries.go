package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

type managedRegistryInput struct {
	Name     string                           `json:"name"`
	Node     string                           `json:"node_id"`
	Project  string                           `json:"project_id"`
	Hostname string                           `json:"hostname"`
	Policy   *worker.ManagedRegistryRetention `json:"retention"`
}

type managedRegistryChangeInput struct {
	Retention     worker.ManagedRegistryRetention `json:"retention"`
	CertificateID *string                         `json:"certificate_id,omitempty"`
}
type managedRegistryRemoveInput struct {
	DeleteData                  bool     `json:"delete_data"`
	AcknowledgeUnavailableNodes []string `json:"acknowledge_unavailable_nodes,omitempty"`
}

type managedRegistryView struct {
	ID             string                          `json:"id"`
	Name           string                          `json:"name"`
	Node           string                          `json:"node_id"`
	Project        string                          `json:"project_id"`
	Hostname       string                          `json:"hostname"`
	State          string                          `json:"state"`
	Connection     string                          `json:"connection_id"`
	Operation      string                          `json:"operation_id"`
	OperationState string                          `json:"operation_state"`
	LastError      string                          `json:"last_error"`
	Policy         worker.ManagedRegistryRetention `json:"retention"`
	Storage        worker.ManagedRegistryStorage   `json:"storage"`
	Image          string                          `json:"image"`
	DataRetained   bool                            `json:"data_retained"`
}

func (s *Server) managedRegistries(w http.ResponseWriter, r *http.Request) {
	team := actor(r).Team
	if r.Method == "GET" {
		rows, err := s.DB.Query(r.Context(), `SELECT m.id,w.name,w.node_id,w.project_id,m.hostname,m.state,COALESCE(m.connection_id,''),COALESCE(m.operation_id,''),COALESCE(o.state,''),m.last_error,m.retention,m.data_retained FROM managed_registries m JOIN workloads w ON w.id=m.id LEFT JOIN operations o ON o.id=m.operation_id WHERE m.team_id=$1 ORDER BY m.created_at`, team)
		if err != nil {
			http.Error(w, "managed registries unavailable", 503)
			return
		}
		defer rows.Close()
		items := []managedRegistryView{}
		for rows.Next() {
			var v managedRegistryView
			var policy []byte
			if err = rows.Scan(&v.ID, &v.Name, &v.Node, &v.Project, &v.Hostname, &v.State, &v.Connection, &v.Operation, &v.OperationState, &v.LastError, &policy, &v.DataRetained); err != nil || json.Unmarshal(policy, &v.Policy) != nil {
				http.Error(w, "managed registries unavailable", 503)
				return
			}
			v.Storage = worker.ManagedRegistryStorageSupport()
			v.Image = worker.ManagedRegistryImage
			items = append(items, v)
		}
		if rows.Err() != nil {
			http.Error(w, "managed registries unavailable", 503)
			return
		}
		canManage := s.teamAdmin(r, team)
		if credential, scoped := r.Context().Value(apiCredentialKey{}).(apiCredential); scoped {
			permitted := false
			for _, route := range s.publicRoutes() {
				registryRoute := route.Path == "/managed-registries" || strings.HasPrefix(route.Path, "/managed-registries/")
				if registryRoute && route.Method != "GET" && containsScope(credential.Scopes, route.Scope) {
					permitted = true
					break
				}
			}
			canManage = canManage && permitted
		}
		respond(w, 200, map[string]any{"registries": items, "can_manage": canManage})
		return
	}
	if !s.teamAdmin(r, team) {
		http.Error(w, "team administrator required", 403)
		return
	}
	if r.Method == "POST" && r.PathValue("id") == "" {
		s.createManagedRegistry(w, r)
		return
	}
	s.changeManagedRegistry(w, r)
}

func (s *Server) createManagedRegistry(w http.ResponseWriter, r *http.Request) {
	var in managedRegistryInput
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Policy == nil {
		in.Policy = &worker.ManagedRegistryRetention{KeepPerRepository: 10, MinimumAgeDays: 30}
	}
	if len(in.Name) == 0 || len(in.Name) > 120 {
		http.Error(w, "a name of at most 120 characters is required", 400)
		return
	}
	if err := in.Policy.Validate(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	project, ok := s.resourceProject(w, r, in.Project, "")
	if !ok {
		return
	}
	id, op := resourceID(), resourceID()
	password := randomToken()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		http.Error(w, "could not protect registry credentials", 500)
		return
	}
	request, err := (worker.ManagedRegistryConfig{ID: id, Hostname: in.Hostname, Username: "cove_registry", Hash: string(hash)}).Deployment(op)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var supported bool
	err = tx.QueryRow(r.Context(), `SELECT state='ready' AND COALESCE((capabilities->>'managed_registry')::boolean,false) AND COALESCE((capabilities->>'file_secrets')::boolean,false) AND ($2 OR EXISTS(SELECT 1 FROM team_nodes WHERE node_id=nodes.id AND team_id=$3)) FROM nodes WHERE id=$1 FOR SHARE`, in.Node, actor(r).Admin, actor(r).Team).Scan(&supported)
	if err != nil || !supported {
		http.Error(w, "select an authorized ready worker with managed registry and file secret support", 409)
		return
	}
	if !s.checkRouteClaims(w, r.Context(), tx, id, in.Node, request.Routes) {
		return
	}
	config := workloadInput{Name: in.Name, NodeID: in.Node, Kind: "service", Compose: request.Compose, Environment: request.Environment, Routes: request.Routes, FileSecrets: request.FileSecrets}
	sealed, err := s.sealJSON(config)
	if err != nil {
		http.Error(w, "could not protect registry configuration", 500)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO workloads(id,node_id,name,configuration,kind,team_id,project_id,next_image_check_at,configured_cpu,configured_memory_mb) VALUES($1,$2,$3,$4,'service',$5,$6,'infinity',1,512)`, id, in.Node, in.Name, sealed, actor(r).Team, project); err != nil {
		if quotaError(w, err) {
			return
		}
		groupError(w, err)
		return
	}
	credentials, err := s.sealJSON(connectionConfig{Name: in.Name + " (" + id[:8] + ")", Registry: &worker.Registry{Server: in.Hostname, Username: "cove_registry", Password: password}})
	if err != nil {
		http.Error(w, "could not protect registry credentials", 500)
		return
	}
	payload, err := s.sealJSON(request)
	if err != nil {
		http.Error(w, "could not protect provisioning operation", 500)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,'deploy',$3)`, op, id, payload); err != nil {
		groupError(w, err)
		return
	}
	policy, _ := json.Marshal(in.Policy)
	if _, err = tx.Exec(r.Context(), `INSERT INTO managed_registries(id,team_id,hostname,credentials,retention,operation_id) VALUES($1,$2,$3,$4,$5,$6)`, id, actor(r).Team, in.Hostname, credentials, policy, op); err != nil {
		groupError(w, err)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not create managed registry", 503)
		return
	}
	respond(w, 202, map[string]string{"id": id, "operation_id": op})
}

func (s *Server) changeManagedRegistry(w http.ResponseWriter, r *http.Request) {
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	id := r.PathValue("id")
	var state, host, node, connection string
	var created time.Time
	var enc, policyJSON, workloadConfig []byte
	err = tx.QueryRow(r.Context(), `SELECT m.state,m.hostname,w.node_id,COALESCE(m.connection_id,''),m.credentials,m.retention,w.configuration,m.created_at FROM managed_registries m JOIN workloads w ON w.id=m.id WHERE m.id=$1 AND m.team_id=$2 FOR UPDATE OF m,w`, id, actor(r).Team).Scan(&state, &host, &node, &connection, &enc, &policyJSON, &workloadConfig, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "managed registry unavailable", 503)
		return
	}
	var busy bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))`, id).Scan(&busy); err != nil {
		http.Error(w, "operations unavailable", 503)
		return
	}
	if busy || state == "maintenance" || state == "removing" {
		http.Error(w, "wait for registry maintenance or provisioning to finish", 409)
		return
	}
	var policy worker.ManagedRegistryRetention
	if json.Unmarshal(policyJSON, &policy) != nil {
		http.Error(w, "registry policy unavailable", 500)
		return
	}
	if r.Method == "PUT" {
		var in managedRegistryChangeInput
		if !decode(w, r, &in) {
			return
		}
		if err = in.Retention.Validate(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if in.CertificateID != nil {
			if state != "failed" {
				http.Error(w, "certificate selection can be changed only before provisioning is verified", 409)
				return
			}
			var cfg workloadInput
			if s.unsealJSON(workloadConfig, &cfg) != nil || len(cfg.Routes) != 1 {
				http.Error(w, "registry configuration unavailable", 500)
				return
			}
			cfg.Routes[0].Policy = nil
			if *in.CertificateID != "" {
				cfg.Routes[0].Policy = &worker.RoutePolicy{CertificateSource: "uploaded", CertificateID: *in.CertificateID}
			}
			if !s.checkRouteClaims(w, r.Context(), tx, id, node, cfg.Routes) {
				return
			}
			sealed, sealErr := s.sealJSON(cfg)
			if sealErr != nil {
				http.Error(w, "could not protect registry configuration", 500)
				return
			}
			if _, err = tx.Exec(r.Context(), `UPDATE workloads SET configuration=$2 WHERE id=$1`, id, sealed); err != nil {
				http.Error(w, "could not save certificate selection", 503)
				return
			}
		}
		raw, _ := json.Marshal(in.Retention)
		if _, err = tx.Exec(r.Context(), `UPDATE managed_registries SET retention=$2 WHERE id=$1`, id, raw); err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			http.Error(w, "could not save registry retention", 503)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	op := resourceID()
	request := worker.Request{ID: op, Workload: id, Action: "registry_maintenance", Kind: "service"}
	var acknowledged []managedRegistryUnavailableNode
	if strings.HasSuffix(r.URL.Path, "/retry") {
		if state != "failed" {
			http.Error(w, "only failed provisioning can be retried", 409)
			return
		}
		var cfg workloadInput
		if s.unsealJSON(workloadConfig, &cfg) != nil {
			http.Error(w, "registry configuration unavailable", 500)
			return
		}
		request = worker.Request{ID: op, Workload: id, Action: "deploy", Kind: "service", Compose: cfg.Compose, Environment: cfg.Environment, Routes: cfg.Routes, FileSecrets: cfg.FileSecrets}
		state = "provisioning"
	} else {
		if state != "ready" && !(r.Method == "DELETE" && (state == "failed" || state == "removed")) {
			http.Error(w, "registry must be ready before maintenance or removal", 409)
			return
		}
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE w.team_id=$1 AND o.state IN ('queued','running'))`, actor(r).Team).Scan(&busy); err != nil || busy {
			http.Error(w, "wait for team operations before taking a complete registry reference inventory", 409)
			return
		}
		mode := "gc"
		deleteData := false
		var protected []string
		if r.Method == "DELETE" {
			mode = "remove"
			var in managedRegistryRemoveInput
			if !decode(w, r, &in) {
				return
			}
			if !validRegistryInventoryAcknowledgement(in.AcknowledgeUnavailableNodes) || !in.DeleteData && len(in.AcknowledgeUnavailableNodes) > 0 {
				http.Error(w, "invalid unavailable-node acknowledgement", 400)
				return
			}
			deleteData = in.DeleteData
			if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM workload_connections WHERE connection_id=$1) OR EXISTS(SELECT 1 FROM registry_webhooks WHERE registry_id=$1)`, connection).Scan(&busy); err != nil || busy {
				http.Error(w, "disconnect resources and registry webhooks before removing this registry", 409)
				return
			}
			if deleteData {
				var unavailable []managedRegistryUnavailableNode
				protected, unavailable, err = s.managedRegistryReferences(r.Context(), tx, actor(r).Team, host, created)
				if err != nil {
					registryInventoryIncomplete(w, err)
					return
				}
				if !registryInventoryAcknowledged(unavailable, in.AcknowledgeUnavailableNodes) {
					registryInventoryChanged(w, unavailable)
					return
				}
				if len(protected) > 0 {
					http.Error(w, "retained releases or builds still reference registry data", 409)
					return
				}
				acknowledged = unavailable
			}
			state = "removing"
		} else {
			var unavailable []managedRegistryUnavailableNode
			protected, unavailable, err = s.managedRegistryReferences(r.Context(), tx, actor(r).Team, host, created)
			if err != nil {
				registryInventoryIncomplete(w, err)
				return
			}
			if len(unavailable) > 0 {
				http.Error(w, "registry reference inventory is incomplete; maintenance refused", 409)
				return
			}
			state = "maintenance"
		}
		request.ManagedRegistry = &worker.ManagedRegistryOperation{Mode: mode, Policy: policy, Protected: protected, DeleteData: deleteData}
	}
	if err = worker.ValidateRequest(request); err != nil {
		http.Error(w, "invalid registry operation", 400)
		return
	}
	payload, err := s.sealJSON(request)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO operations(id,workload_id,action,payload) VALUES($1,$2,$3,$4)`, op, id, request.Action, payload)
	}
	if err == nil {
		err = recordRegistryInventoryAcknowledgement(r.Context(), tx, id, op, actor(r).ID, actor(r).Team, acknowledged)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE managed_registries SET previous_state=state,state=$2,operation_id=$3,last_error='',verification_attempts=0,next_verification_at=NULL WHERE id=$1`, id, state, op)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		groupError(w, err)
		return
	}
	respond(w, 202, map[string]string{"operation_id": op})
}

func (s *Server) managedRegistryReferences(ctx context.Context, tx pgx.Tx, team, server string, created time.Time) ([]string, []managedRegistryUnavailableNode, error) {
	scopes, err := managedRegistryReferenceScopes(ctx, tx, team)
	if err != nil {
		return nil, nil, err
	}
	unavailable := unavailableRegistryNodes(scopes)
	refs := map[string]bool{}
	for _, v := range scopes {
		if v.never || !v.ready {
			continue
		}
		previous, err := registryPreviousReleases(ctx, tx, v.id, created)
		if err != nil {
			return nil, nil, registryWorkerInventoryFailure(v, err)
		}
		result, err := s.nodeRPC(ctx, v.node, worker.RPCRequest{Method: "managed_registry_references", ID: v.id, Registry: &worker.Registry{Server: server}, RegistryPrevious: previous})
		if err != nil {
			return nil, nil, registryWorkerInventoryFailure(v, err)
		}
		if result.Error != "" {
			return nil, nil, registryWorkerInventoryFailure(v, errors.New(result.Error))
		}
		for _, ref := range result.RegistryReferences {
			refs[ref] = true
		}
		if len(refs) > worker.MaxManagedRegistryReferences {
			return nil, nil, errors.New("registry reference inventory exceeds bound")
		}
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (o.workload_id) o.result FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE w.team_id=$1 AND o.action='build' AND o.state='succeeded' ORDER BY o.workload_id,o.created_at DESC LIMIT 1025`, team)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > 1024 {
			return nil, nil, errors.New("build inventory exceeds bound")
		}
		var raw []byte
		if rows.Scan(&raw) != nil {
			return nil, nil, errors.New("build inventory unavailable")
		}
		var op worker.Operation
		if json.Unmarshal(raw, &op) != nil || op.Build == nil {
			return nil, nil, errors.New("build inventory incomplete")
		}
		for _, image := range op.Build.Images {
			if strings.HasPrefix(image.Reference, server+"/") {
				refs[strings.TrimPrefix(image.Reference, server+"/")] = true
			}
		}
		if len(refs) > worker.MaxManagedRegistryReferences {
			return nil, nil, errors.New("registry reference inventory exceeds bound")
		}
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	rows.Close()
	ciReferences, err := s.previewCIRegistryReferences(ctx, tx, team, server)
	if err != nil {
		return nil, nil, err
	}
	for _, ref := range ciReferences {
		refs[ref] = true
	}
	if len(refs) > worker.MaxManagedRegistryReferences {
		return nil, nil, errors.New("registry reference inventory exceeds bound")
	}
	result := make([]string, 0, len(refs))
	for ref := range refs {
		result = append(result, ref)
	}
	sort.Strings(result)
	return result, unavailable, nil
}

func (s *Server) reconcileManagedRegistries(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT m.id,m.state,m.connection_id,m.credentials,o.state,o.result,w.node_id,o.id,o.payload FROM managed_registries m JOIN operations o ON o.id=m.operation_id JOIN workloads w ON w.id=m.id WHERE (m.state IN ('provisioning','maintenance','removing') OR m.state='failed' AND m.next_verification_at IS NOT NULL) AND o.state NOT IN ('queued','running') AND (m.next_verification_at IS NULL OR m.next_verification_at<=now()) ORDER BY m.created_at LIMIT 4`)
	if err != nil {
		return err
	}
	type item struct {
		id, state           string
		connection          *string
		credentials, result []byte
		operationState      string
		node, operation     string
		payload             []byte
	}
	var list []item
	for rows.Next() {
		var v item
		if err = rows.Scan(&v.id, &v.state, &v.connection, &v.credentials, &v.operationState, &v.result, &v.node, &v.operation, &v.payload); err != nil {
			rows.Close()
			return err
		}
		list = append(list, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range list {
		if v.state != "provisioning" && v.state != "failed" {
			var original worker.Request
			if err = s.unsealJSON(v.payload, &original); err != nil {
				return err
			}
			recovery, stop := context.WithTimeout(ctx, 30*time.Second)
			status, readErr := s.nodeRPC(recovery, v.node, worker.RPCRequest{Method: "managed_registry_recover", ID: v.operation, Request: &original})
			stop()
			if readErr != nil || status.Operation == nil || status.Operation.RegistryRecoveryPending {
				continue
			}
			if status.Operation.ManagedRegistry != nil && status.Operation.ManagedRegistry.Removed {
				v.operationState = "succeeded"
				v.result, err = json.Marshal(status.Operation)
				if err != nil {
					return err
				}
			}
		}
		if v.operationState != "succeeded" {
			if _, err = s.DB.Exec(ctx, `UPDATE managed_registries SET state=CASE WHEN state='provisioning' THEN 'failed' WHEN previous_state IN ('ready','failed','removed') THEN previous_state ELSE 'failed' END,previous_state='',last_error='The registry operation failed or was interrupted; inspect its operation before retrying.' WHERE id=$1 AND state=$2`, v.id, v.state); err != nil {
				return err
			}
			continue
		}
		if v.state == "provisioning" || v.state == "failed" {
			var credentials connectionConfig
			if err = s.unsealJSON(v.credentials, &credentials); err != nil {
				return err
			}
			var node string
			if err = s.DB.QueryRow(ctx, `SELECT node_id FROM workloads WHERE id=$1`, v.id).Scan(&node); err != nil {
				return err
			}
			var configData []byte
			if err = s.DB.QueryRow(ctx, `SELECT configuration FROM workloads WHERE id=$1`, v.id).Scan(&configData); err != nil {
				return err
			}
			var config workloadInput
			if err = s.unsealJSON(configData, &config); err != nil {
				return err
			}
			selectedCertificate := ""
			if len(config.Routes) != 1 {
				return errors.New("registry route configuration is incomplete")
			}
			if policy := config.Routes[0].Policy; policy != nil && policy.CertificateID != "" {
				material, _, certificateErr := s.routeCertificate(ctx, s.DB, v.id, policy.CertificateID)
				if certificateErr != nil {
					return certificateErr
				}
				selectedCertificate = material.CertificatePEM
			}
			probe, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, err = s.nodeRPC(probe, node, worker.RPCRequest{Method: "managed_registry_verify", Registry: credentials.Registry, RegistryCertificate: selectedCertificate})
			cancel()
			if err != nil {
				message, retry := registryVerificationFailure(err)
				if retry {
					err = s.deferRegistryVerification(ctx, v.id, v.operation, message)
				} else {
					_, err = s.DB.Exec(ctx, `UPDATE managed_registries SET state='failed',last_error=$2,next_verification_at=NULL WHERE id=$1 AND operation_id=$3 AND state IN ('provisioning','failed')`, v.id, message, v.operation)
				}
				if err != nil {
					return err
				}
				continue
			}
			tx, err := s.DB.Begin(ctx)
			if err != nil {
				return err
			}
			connection := resourceID()
			_, err = tx.Exec(ctx, `INSERT INTO infrastructure_connections(id,team_id,kind,name,configuration) SELECT $2,team_id,'registry',$3,credentials FROM managed_registries WHERE id=$1 AND operation_id=$4 AND state IN ('provisioning','failed') FOR UPDATE`, v.id, connection, credentials.Name, v.operation)
			if err == nil {
				_, err = tx.Exec(ctx, `UPDATE managed_registries SET state='ready',connection_id=$2,last_error='',next_verification_at=NULL,verification_attempts=0 WHERE id=$1 AND operation_id=$3 AND state IN ('provisioning','failed')`, v.id, connection, v.operation)
			}
			if err == nil {
				err = tx.Commit(ctx)
			}
			_ = tx.Rollback(ctx)
			if err != nil {
				return err
			}
		} else if v.state == "maintenance" {
			_, err = s.DB.Exec(ctx, `UPDATE managed_registries SET state='ready',last_error='' WHERE id=$1 AND state='maintenance'`, v.id)
			if err != nil {
				return err
			}
		} else {
			var result worker.Operation
			if json.Unmarshal(v.result, &result) != nil || result.ManagedRegistry == nil || !result.ManagedRegistry.Removed {
				return errors.New("registry removal result is incomplete")
			}
			tx, err := s.DB.Begin(ctx)
			if err != nil {
				return err
			}
			var encrypted []byte
			err = tx.QueryRow(ctx, `SELECT configuration FROM workloads WHERE id=$1 FOR UPDATE`, v.id).Scan(&encrypted)
			var config workloadInput
			if err == nil {
				err = s.unsealJSON(encrypted, &config)
			}
			if err == nil {
				config.Routes = nil
				config.Environment = nil
				config.FileSecrets = nil
				encrypted, err = s.sealJSON(config)
			}
			if err == nil {
				_, err = tx.Exec(ctx, `UPDATE workloads SET configuration=$2,configured_cpu=NULL,configured_memory_mb=NULL,manually_stopped=true WHERE id=$1`, v.id, encrypted)
			}
			if err == nil {
				_, err = tx.Exec(ctx, `UPDATE managed_registries SET state='removed',data_retained=$2,connection_id=NULL,credentials=''::bytea,last_error='' WHERE id=$1`, v.id, result.ManagedRegistry.DataRetained)
			}
			if err == nil && v.connection != nil {
				_, err = tx.Exec(ctx, `DELETE FROM infrastructure_connections WHERE id=$1`, *v.connection)
			}
			if err == nil {
				err = tx.Commit(ctx)
			}
			_ = tx.Rollback(ctx)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
