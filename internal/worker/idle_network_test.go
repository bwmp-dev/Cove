package worker

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestIdleAndLinkNetworksShareThePoolWithoutSharingOwnership(t *testing.T) {
	t.Setenv("COVE_LINK_POOL", "10.250.0.0/16")
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var live []dockerNetwork
	if err = json.Unmarshal([]byte(`[{"Name":"existing","IPAM":{"Config":[{"Subnet":"10.250.0.0/24"}]}}]`), &live); err != nil {
		t.Fatal(err)
	}
	created := 0
	e.Run = func(_ context.Context, _ string, args, _ []string, out io.Writer) error {
		switch args[2] {
		case "ls":
			for _, n := range live {
				if filter := slices.Index(args, "--filter"); filter >= 0 {
					value := args[filter+1]
					if strings.HasPrefix(value, "name=^") && n.Name != strings.TrimSuffix(strings.TrimPrefix(value, "name=^"), "$") {
						continue
					}
					if strings.HasPrefix(value, "label=cove.managed=") && n.Labels["cove.managed"] != strings.TrimPrefix(value, "label=cove.managed=") {
						continue
					}
				}
				io.WriteString(out, n.Name+"\n")
			}
		case "inspect":
			selected := []dockerNetwork{}
			for _, n := range live {
				if slices.Contains(args[3:], n.Name) {
					selected = append(selected, n)
				}
			}
			return json.NewEncoder(out).Encode(selected)
		case "create":
			created++
			var n dockerNetwork
			n.Name, n.Labels = args[len(args)-1], map[string]string{}
			for i, arg := range args {
				if arg == "--label" {
					key, value, _ := strings.Cut(args[i+1], "=")
					n.Labels[key] = value
				}
				if arg == "--subnet" {
					n.IPAM.Config = append(n.IPAM.Config, struct{ Subnet string }{Subnet: args[i+1]})
				}
			}
			live = append(live, n)
		default:
			t.Fatal(args)
		}
		return nil
	}
	idle := map[string]*linkNetworkInfo{}
	front, err := e.ensureHelperNetwork(context.Background(), idle, "front", "idle", "front", "app", 29)
	if err != nil || front.Subnet != "10.250.1.0/29" {
		t.Fatal(front, err)
	}
	back, err := e.ensureHelperNetwork(context.Background(), idle, "back", "idle", "back", "app", 24)
	if err != nil || back.Subnet != "10.250.2.0/24" {
		t.Fatal(back, err)
	}
	links := map[string]*linkNetworkInfo{}
	link, err := e.ensureLinkNetwork(context.Background(), links, "link", "source", "app")
	if err != nil || link.Subnet != "10.250.1.32/27" {
		t.Fatal(link, err)
	}
	owned, err := e.linkNetworks(context.Background())
	if err != nil || len(owned) != 1 || owned["link"] == nil {
		t.Fatal("link reconciliation could adopt idle networks", owned, err)
	}
	if _, err = e.ensureHelperNetwork(context.Background(), idle, "front", "idle", "front", "another-app", 29); err == nil || created != 3 {
		t.Fatal("another workload adopted the network", created, err)
	}
	live = live[:1]
	live[0].IPAM.Config[0].Subnet = "0.0.0.0/0"
	if _, err = e.ensureHelperNetwork(context.Background(), map[string]*linkNetworkInfo{}, "exhausted", "idle", "front", "app", 29); err == nil || created != 3 {
		t.Fatal("overlapping pool allocated an address", created, err)
	}
}
