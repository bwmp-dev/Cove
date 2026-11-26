package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"

	"github.com/jackc/pgx/v5"
)

type managedRegistryUnavailableNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type managedRegistryRemovalInventory struct {
	UnavailableNodes []managedRegistryUnavailableNode `json:"unavailable_nodes"`
}

type managedRegistryReferenceScope struct {
	id, node, name string
	ready, never   bool
}

func managedRegistryReferenceScopes(ctx context.Context, tx pgx.Tx, team string) ([]managedRegistryReferenceScope, error) {
	// SSH enrollment pins a host identity before worker RPC is possible, and
	// normal node updates never clear that pin. Reachability alone proves nothing.
	rows, err := tx.Query(ctx, `SELECT w.id,n.id,n.name,n.state='ready',n.mode='ssh' AND n.host_key IS NULL AND COALESCE(n.capabilities,'null'::jsonb)='null'::jsonb FROM workloads w JOIN nodes n ON n.id=w.node_id WHERE w.team_id=$1 AND w.id NOT IN (SELECT id FROM managed_registries) ORDER BY w.id LIMIT 1025 FOR SHARE OF n`, team)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scopes := []managedRegistryReferenceScope{}
	for rows.Next() {
		var scope managedRegistryReferenceScope
		if err = rows.Scan(&scope.id, &scope.node, &scope.name, &scope.ready, &scope.never); err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(scopes) > 1024 {
		return nil, errors.New("registry resource inventory exceeds bound")
	}
	return scopes, nil
}

func unavailableRegistryNodes(scopes []managedRegistryReferenceScope) []managedRegistryUnavailableNode {
	nodes := map[string]string{}
	for _, scope := range scopes {
		if !scope.ready && !scope.never {
			nodes[scope.node] = scope.name
		}
	}
	result := []managedRegistryUnavailableNode{}
	for id, name := range nodes {
		result = append(result, managedRegistryUnavailableNode{ID: id, Name: name})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (s *Server) managedRegistryRemovalInventory(w http.ResponseWriter, r *http.Request) {
	if !s.teamAdmin(r, actor(r).Team) {
		http.Error(w, "team administrator required", 403)
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "registry removal inventory unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var exists bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM managed_registries WHERE id=$1 AND team_id=$2)`, r.PathValue("id"), actor(r).Team).Scan(&exists); err != nil {
		http.Error(w, "registry removal inventory unavailable", 503)
		return
	}
	if !exists {
		http.NotFound(w, r)
		return
	}
	scopes, err := managedRegistryReferenceScopes(r.Context(), tx, actor(r).Team)
	if err != nil {
		http.Error(w, "registry removal inventory unavailable", 503)
		return
	}
	respond(w, 200, managedRegistryRemovalInventory{UnavailableNodes: unavailableRegistryNodes(scopes)})
}

func validRegistryInventoryAcknowledgement(ids []string) bool {
	if len(ids) > 1024 {
		return false
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || len(id) > 128 || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func registryInventoryAcknowledged(nodes []managedRegistryUnavailableNode, ids []string) bool {
	current := make([]string, len(nodes))
	for i, node := range nodes {
		current[i] = node.ID
	}
	provided := slices.Clone(ids)
	slices.Sort(provided)
	return slices.Equal(current, provided)
}

func registryInventoryChanged(w http.ResponseWriter, nodes []managedRegistryUnavailableNode) {
	respond(w, 409, map[string]any{"error": "registry_inventory_changed", "message": "Review and acknowledge the unavailable nodes before deleting registry data.", "unavailable_nodes": nodes})
}

func recordRegistryInventoryAcknowledgement(ctx context.Context, tx pgx.Tx, registry, operation, by, team string, nodes []managedRegistryUnavailableNode) error {
	if len(nodes) == 0 {
		return nil
	}
	reason := fmt.Sprintf("Registry data deletion acknowledged incomplete inventory on %d unavailable nodes.", len(nodes))
	if _, err := tx.Exec(ctx, `UPDATE operations SET reason=$2 WHERE id=$1`, operation, reason); err != nil {
		return err
	}
	details, err := json.Marshal(map[string]any{"operation_id": operation, "unavailable_nodes": nodes})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events(actor,action,subject,status,team_id,details) VALUES($1,'registry data deletion acknowledgement',$2,202,$3,$4)`, by, registry, team, details)
	return err
}
