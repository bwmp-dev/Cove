package control

import "net/http"

func (s *Server) dependencyMap(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(r.Context(), `
 WITH visible AS (
  SELECT id,name,project_id FROM workloads w WHERE team_id=$2 AND ($4 OR cove_access($3,w.team_id,w.project_id,COALESCE(w.environment_id,''))>=1)
 ), edges AS (
  SELECT workload_id AS source,source_id AS target,'variable' AS kind,name AS label FROM resource_variable_bindings
  UNION ALL
  SELECT source_id,target_id,'connection',alias||':'||port FROM resource_connections WHERE NOT removing
  UNION ALL
  SELECT a.workload_id,b.workload_id,'network',n.name FROM workload_networks a JOIN workload_networks b ON b.network_id=a.network_id AND a.workload_id<b.workload_id JOIN shared_networks n ON n.id=a.network_id
  UNION ALL
  SELECT step.value,previous.value,'order','Deploys after' FROM deployment_sequences s CROSS JOIN LATERAL jsonb_array_elements_text(s.workload_ids) WITH ORDINALITY step CROSS JOIN LATERAL jsonb_array_elements_text(s.workload_ids) WITH ORDINALITY previous WHERE step.ordinality=previous.ordinality+1
 ) SELECT DISTINCT a.id,a.name,b.id,b.name,e.kind,e.label FROM edges e JOIN visible a ON a.id=e.source JOIN visible b ON b.id=e.target WHERE a.project_id=$1 OR b.project_id=$1 ORDER BY a.name,b.name,e.kind,e.label`, r.PathValue("id"), actor(r).Team, actor(r).ID, actor(r).Admin)
	if err != nil {
		http.Error(w, "dependencies unavailable", 503)
		return
	}
	defer rows.Close()
	type edge struct {
		Source     string `json:"source"`
		SourceName string `json:"source_name"`
		Target     string `json:"target"`
		TargetName string `json:"target_name"`
		Kind       string `json:"kind"`
		Label      string `json:"label"`
	}
	edges := []edge{}
	for rows.Next() {
		var e edge
		if err = rows.Scan(&e.Source, &e.SourceName, &e.Target, &e.TargetName, &e.Kind, &e.Label); err != nil {
			http.Error(w, "dependencies unavailable", 503)
			return
		}
		edges = append(edges, e)
	}
	if rows.Err() != nil {
		http.Error(w, "dependencies unavailable", 503)
		return
	}
	respond(w, 200, edges)
}
