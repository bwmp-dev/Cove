package control

import (
	"context"
	"cove/internal/worker"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Server) firewallManagementPorts(ctx context.Context) ([]uint16, error) {
	rows, err := s.DB.Query(ctx, `SELECT port FROM nodes UNION SELECT port FROM ssh_bastions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ports := []uint16{22, 80, 443}
	for rows.Next() {
		var port int
		if rows.Scan(&port) != nil || port < 1 || port > 65535 {
			return nil, errors.New("management port inventory is invalid")
		}
		ports = append(ports, uint16(port))
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	origin, err := url.Parse(s.PublicURL)
	if err != nil || origin.Scheme != "https" && origin.Scheme != "http" || origin.Hostname() == "" {
		return nil, errors.New("panel management origin is invalid")
	}
	if origin.Port() != "" {
		port, err := strconv.ParseUint(origin.Port(), 10, 16)
		if err != nil || port == 0 {
			return nil, errors.New("panel management port is invalid")
		}
		ports = append(ports, uint16(port))
	}
	slices.Sort(ports)
	return slices.Compact(ports), nil
}

func (s *Server) nodeFirewall(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	control := worker.FirewallControl{Action: "inspect"}
	if r.Method != http.MethodGet {
		var in struct {
			Remove   bool                  `json:"remove"`
			Rules    []worker.FirewallRule `json:"rules"`
			Revision string                `json:"revision"`
			Confirm  bool                  `json:"confirm"`
			Revert   bool                  `json:"revert"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.Remove && (len(in.Rules) != 0 || in.Revert) {
			http.Error(w, "stop managing the firewall separately from rules or a revert", 400)
			return
		}
		control.Remove = in.Remove
		if len(in.Rules) > 32 {
			http.Error(w, "at most 32 application-port rules are allowed", 400)
			return
		}
		control.Action, control.Rules, control.Revision = "preview", in.Rules, in.Revision
		if r.Pattern == "POST /api/nodes/{id}/firewall" {
			if !in.Confirm {
				http.Error(w, "confirm the firewall change", 400)
				return
			}
			control.Action = "apply"
			if in.Revert {
				control.Action = "revert"
			} else if digest, err := hex.DecodeString(in.Revision); err != nil || len(digest) != 32 {
				http.Error(w, "preview the current firewall policy first", 400)
				return
			}
		}
	}
	var ready, capable, removalCapable, busy bool
	err := s.DB.QueryRow(r.Context(), `SELECT state='ready',COALESCE((capabilities->>'firewall')::boolean,false),COALESCE((capabilities->>'firewall_remove')::boolean,false),EXISTS(SELECT 1 FROM workloads w JOIN operations o ON o.workload_id=w.id WHERE w.node_id=n.id AND o.state IN ('queued','running')) OR EXISTS(SELECT 1 FROM node_identity_rotations i WHERE i.node_id=n.id AND i.phase NOT IN ('succeeded','cancelled')) FROM nodes n WHERE id=$1`, r.PathValue("id")).Scan(&ready, &capable, &removalCapable, &busy)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "node firewall state unavailable", 503)
		return
	}
	if !ready || !capable || busy {
		http.Error(w, "verify an upgraded Linux worker and wait for operations and SSH identity changes", 409)
		return
	}
	if control.Remove && !removalCapable {
		http.Error(w, "upgrade this worker before stopping firewall management", 409)
		return
	}
	control.Reserved, err = s.firewallManagementPorts(r.Context())
	if err != nil {
		http.Error(w, "management port inventory unavailable; firewall changes are refused", 503)
		return
	}
	response, err := s.nodeRPC(r.Context(), r.PathValue("id"), worker.RPCRequest{Method: "firewall", Firewall: &control})
	if err != nil || response.Error != "" || response.Firewall == nil {
		http.Error(w, "firewall request could not be confirmed; refresh and inspect independent recovery before retrying", 502)
		return
	}
	if control.Action != "apply" {
		respond(w, 200, response.Firewall)
		return
	}
	if response.Firewall.Pending == nil || !response.Firewall.Supported {
		http.Error(w, "host firewall prerequisites or a confirmed pending change are unavailable; inspect its status", 409)
		return
	}
	intent := response.Firewall.Pending.ID
	probeContext, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	probe, probeErr := s.nodeRPC(probeContext, r.PathValue("id"), worker.RPCRequest{Method: "probe"})
	cancel()
	if probeErr != nil || probe.Probe == nil || probe.Probe.Protocol != 1 || !probe.Probe.Ready {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
		defer cancel()
		control.Action, control.Intent = "revert", intent
		reverted, revertErr := s.nodeRPC(cleanup, r.PathValue("id"), worker.RPCRequest{Method: "firewall", Firewall: &control})
		if revertErr != nil || reverted.Error != "" || reverted.Firewall == nil || reverted.Firewall.Pending != nil {
			http.Error(w, "fresh worker connectivity and rollback could not be confirmed; inspect the independent timed recovery deadline", 502)
		} else {
			http.Error(w, "the firewall change was reverted because fresh worker connectivity was not confirmed", 502)
		}
		return
	}
	control.Action, control.Intent = "confirm", intent
	response, err = s.nodeRPC(r.Context(), r.PathValue("id"), worker.RPCRequest{Method: "firewall", Firewall: &control})
	if err != nil || response.Error != "" || response.Firewall == nil || response.Firewall.Pending != nil {
		http.Error(w, "firewall confirmation could not be verified; refresh before retrying and inspect the recovery deadline", 502)
		return
	}
	respond(w, 200, response.Firewall)
}
