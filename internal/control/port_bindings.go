package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"

	"go.yaml.in/yaml/v3"
)

func rewritePortBindings(compose, address string) (string, int, error) {
	ip, err := netip.ParseAddr(address)
	if err != nil || ip.IsUnspecified() || ip.IsMulticast() || ip.Zone() != "" {
		return "", 0, errors.New("a specific unicast bind address is required")
	}
	var root yaml.Node
	if yaml.Unmarshal([]byte(compose), &root) != nil || len(root.Content) != 1 {
		return "", 0, errors.New("invalid Compose document")
	}
	field := func(node *yaml.Node, name string) *yaml.Node {
		if node.Kind != yaml.MappingNode {
			return nil
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == name {
				return node.Content[i+1]
			}
		}
		return nil
	}
	services := field(root.Content[0], "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return "", 0, errors.New("Compose services required")
	}
	var rejectAliases func(*yaml.Node) bool
	rejectAliases = func(n *yaml.Node) bool {
		if n.Kind == yaml.AliasNode {
			return true
		}
		for _, child := range n.Content {
			if rejectAliases(child) {
				return true
			}
		}
		return false
	}
	if rejectAliases(&root) {
		return "", 0, errors.New("expand Compose aliases before rewriting ports")
	}
	changed := 0
	for i := 1; i < len(services.Content); i += 2 {
		service := services.Content[i]
		if service.Alias != nil {
			return "", 0, errors.New("expand Compose aliases before rewriting ports")
		}
		if field(service, "<<") != nil {
			return "", 0, errors.New("expand Compose merges before rewriting ports")
		}
		ports := field(service, "ports")
		if ports == nil {
			continue
		}
		if ports.Kind != yaml.SequenceNode {
			return "", 0, errors.New("expand Compose port aliases before rewriting")
		}
		for _, port := range ports.Content {
			if port.Kind == yaml.MappingNode {
				if field(port, "<<") != nil {
					return "", 0, errors.New("expand Compose port merges before rewriting")
				}
				host := field(port, "host_ip")
				if host == nil {
					port.Content = append(port.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "host_ip"}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ip.String()})
					changed++
				} else if host.Value == "" || host.Value == "0.0.0.0" || host.Value == "::" {
					host.Value = ip.String()
					changed++
				}
			} else if port.Kind == yaml.ScalarNode {
				v := port.Value
				if strings.Contains(v, "$") {
					return "", 0, errors.New("resolve interpolated port bindings before rewriting")
				}
				if strings.HasPrefix(v, "[::]:") {
					v = strings.TrimPrefix(v, "[::]:")
				} else if strings.HasPrefix(v, "0.0.0.0:") {
					v = strings.TrimPrefix(v, "0.0.0.0:")
				} else if strings.Count(v, ":") > 1 {
					continue
				}
				if !strings.Contains(v, ":") {
					v = ":" + v
				}
				host := ip.String()
				if ip.Is6() {
					host = "[" + host + "]"
				}
				port.Tag = "!!str"
				port.Value = host + ":" + v
				changed++
			} else {
				return "", 0, errors.New("expand Compose port aliases before rewriting")
			}
		}
	}
	if changed == 0 {
		return compose, 0, nil
	}
	data, err := yaml.Marshal(&root)
	return string(data), changed, err
}

func (s *Server) portBindings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Address  string `json:"address"`
		Confirm  bool   `json:"confirm"`
		Revision string `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Address == "" {
		in.Address = "127.0.0.1"
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "configuration unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	var encrypted []byte
	if err = tx.QueryRow(r.Context(), "SELECT configuration FROM workloads WHERE id=$1 FOR UPDATE", r.PathValue("id")).Scan(&encrypted); err != nil {
		http.Error(w, "configuration unavailable", 503)
		return
	}
	data, err := unseal(s.Key, encrypted)
	var c workloadInput
	if err != nil || json.Unmarshal(data, &c) != nil {
		http.Error(w, "configuration unavailable", 503)
		return
	}
	if c.Database != nil {
		http.Error(w, "use database public-access settings to change managed port bindings", 409)
		return
	}
	if c.Source != nil && !c.Source.InlineCompose {
		http.Error(w, "edit port bindings in the Git repository, or switch to inline Compose", 409)
		return
	}
	sum := sha256.Sum256([]byte(c.Compose))
	revision := hex.EncodeToString(sum[:])
	compose, count, err := rewritePortBindings(c.Compose, in.Address)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if in.Confirm {
		if revision != in.Revision {
			http.Error(w, "Compose changed; preview the rewrite again", 409)
			return
		}
		var active bool
		if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM operations WHERE workload_id=$1 AND state IN ('queued','running'))", r.PathValue("id")).Scan(&active); err != nil {
			http.Error(w, "operations unavailable", 503)
			return
		}
		if active {
			http.Error(w, "wait for the active operation before editing", 409)
			return
		}
		c.Compose = compose
		if err = validateWorkload(c); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		data, err = json.Marshal(c)
		if err == nil {
			data, err = seal(s.Key, data)
		}
		if err == nil {
			_, err = tx.Exec(r.Context(), "UPDATE workloads SET configuration=$1 WHERE id=$2", data, r.PathValue("id"))
		}
		if err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			http.Error(w, "could not save Compose", 500)
			return
		}
	}
	respond(w, 200, map[string]any{"compose": compose, "changed": count, "revision": revision, "saved": in.Confirm})
}
