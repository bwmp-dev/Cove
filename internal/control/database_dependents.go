package control

import "net/http"

type databaseDependent struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	NeedsRedeploy bool   `json:"needs_redeploy"`
}

func (s *Server) databaseDependents(r *http.Request, id string) ([]databaseDependent, int, error) {
	rows, err := s.DB.Query(r.Context(), `WITH changed AS (SELECT MAX(at) AS at FROM (
 SELECT updated_at AS at FROM database_maintenance WHERE workload_id=$1 AND kind='rotate' AND state='succeeded'
 UNION ALL SELECT cutover_at FROM database_maintenance WHERE workload_id=$1 AND cutover_at IS NOT NULL
 UNION ALL SELECT o.updated_at FROM operations o JOIN workloads w ON w.id=o.workload_id WHERE w.id=$1 AND o.state='succeeded' AND o.id=(SELECT id FROM operations WHERE workload_id=$1 AND action='deploy' ORDER BY created_at DESC LIMIT 1)
 ) changes) SELECT DISTINCT w.id,w.name,COALESCE((SELECT MAX(created_at) FROM operations WHERE workload_id=w.id AND action='deploy' AND state='succeeded') < changed.at,true) FROM resource_variable_bindings b JOIN workloads w ON w.id=b.workload_id CROSS JOIN changed WHERE b.source_id=$1 AND w.team_id=$2 ORDER BY w.name,w.id`, id, actor(r).Team)
	if err != nil {
		return nil, 0, err
	}
	all := []databaseDependent{}
	for rows.Next() {
		var item databaseDependent
		if err = rows.Scan(&item.ID, &item.Name, &item.NeedsRedeploy); err != nil {
			rows.Close()
			return nil, 0, err
		}
		all = append(all, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	visible := []databaseDependent{}
	hidden := 0
	for _, item := range all {
		allowed, err := s.permission(r, "workload", item.ID, "view")
		if err != nil {
			return nil, 0, err
		}
		if allowed {
			visible = append(visible, item)
		} else {
			hidden++
		}
	}
	return visible, hidden, nil
}
