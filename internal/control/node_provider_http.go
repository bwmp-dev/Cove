package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type nodeProvider struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	HasToken bool   `json:"has_token"`
	Token    string `json:"-"`
}

type providerOption struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Architecture string   `json:"architecture,omitempty"`
	CPUs         int      `json:"cpus,omitempty"`
	MemoryMB     int64    `json:"memory_mb,omitempty"`
	Monthly      string   `json:"monthly,omitempty"`
	Currency     string   `json:"currency,omitempty"`
	Regions      []string `json:"regions,omitempty"`
}

type providerCatalog struct {
	Regions   []providerOption `json:"regions"`
	Plans     []providerOption `json:"plans"`
	Images    []providerOption `json:"images"`
	Limited   bool             `json:"limited"`
	CheckedAt time.Time        `json:"checked_at"`
}

type providerInstance struct {
	ID      string
	Name    string
	Address string
	State   string
	Owned   bool
}

type providerHTTPError struct{ Status int }

func (e *providerHTTPError) Error() string {
	return fmt.Sprintf("cloud provider returned HTTP %d", e.Status)
}

var providerValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

func providerOrigin(kind string) string {
	switch kind {
	case "hetzner":
		return "https://api.hetzner.cloud/v1"
	case "digitalocean":
		return "https://api.digitalocean.com/v2"
	case "vultr":
		return "https://api.vultr.com/v2"
	}
	return ""
}

func (s *Server) providerJSON(ctx context.Context, provider nodeProvider, method, path string, body any) (map[string]json.RawMessage, error) {
	origin := providerOrigin(provider.Kind)
	if origin == "" || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\r\n#") {
		return nil, errors.New("invalid cloud provider request")
	}
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil || len(data) > 65536 {
			return nil, errors.New("cloud creation request exceeds its limit")
		}
		input = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, origin+path, input)
	if err != nil {
		return nil, errors.New("cloud request could not be constructed")
	}
	request.Header.Set("Authorization", "Bearer "+provider.Token)
	request.Header.Set("Content-Type", "application/json")
	client := http.Client{Transport: s.ProviderTransport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("cloud provider request could not be confirmed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &providerHTTPError{Status: response.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return nil, errors.New("cloud response exceeds its bounded size")
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(data, &value) != nil {
		return nil, errors.New("cloud provider returned invalid JSON")
	}
	return value, nil
}

func providerText(row map[string]json.RawMessage, key string) string {
	var value string
	if json.Unmarshal(row[key], &value) != nil {
		var number json.Number
		if json.Unmarshal(row[key], &number) != nil {
			return ""
		}
		value = number.String()
	}
	if len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	return value
}

func providerInt(row map[string]json.RawMessage, key string) int64 {
	value, err := strconv.ParseInt(providerText(row, key), 10, 64)
	if err != nil || value < 0 || value > 16777216 {
		return 0
	}
	return value
}

func (s *Server) providerRows(ctx context.Context, provider nodeProvider, path, key string, query url.Values) ([]map[string]json.RawMessage, bool, error) {
	rows := []map[string]json.RawMessage{}
	cursor := ""
	for page := 1; page <= 6; page++ {
		values := url.Values{}
		for key, list := range query {
			values[key] = slices.Clone(list)
		}
		values.Set("per_page", "100")
		if provider.Kind == "vultr" {
			if cursor != "" {
				values.Set("cursor", cursor)
			}
		} else {
			values.Set("page", strconv.Itoa(page))
		}
		response, err := s.providerJSON(ctx, provider, "GET", path+"?"+values.Encode(), nil)
		if err != nil {
			return nil, false, err
		}
		var items []map[string]json.RawMessage
		if json.Unmarshal(response[key], &items) != nil || len(items) > 100 {
			return nil, false, errors.New("cloud inventory page is invalid or oversized")
		}
		if len(rows)+len(items) > 512 {
			return append(rows, items[:512-len(rows)]...), true, nil
		}
		rows = append(rows, items...)
		if provider.Kind == "vultr" {
			var meta struct {
				Links struct {
					Next string `json:"next"`
				} `json:"links"`
			}
			if json.Unmarshal(response["meta"], &meta) != nil {
				return nil, false, errors.New("invalid cloud inventory cursor")
			}
			if meta.Links.Next == "" {
				return rows, false, nil
			}
			if len(meta.Links.Next) > 1024 || meta.Links.Next == cursor {
				return nil, false, errors.New("cloud inventory cursor is invalid or repeated")
			}
			cursor = meta.Links.Next
		} else if len(items) < 100 {
			return rows, false, nil
		}
	}
	return rows, true, nil
}

func (s *Server) cloudCatalog(ctx context.Context, provider nodeProvider) (providerCatalog, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	result := providerCatalog{Regions: []providerOption{}, Plans: []providerOption{}, Images: []providerOption{}, CheckedAt: time.Now().UTC()}
	type collection struct {
		path, key, target string
		query             url.Values
	}
	sets := []collection{}
	switch provider.Kind {
	case "hetzner":
		sets = []collection{{"/locations", "locations", "regions", nil}, {"/server_types", "server_types", "plans", nil}, {"/images", "images", "images", url.Values{"type": {"snapshot"}}}}
	case "digitalocean":
		sets = []collection{{"/regions", "regions", "regions", nil}, {"/sizes", "sizes", "plans", nil}, {"/images", "images", "images", url.Values{"private": {"true"}}}}
	case "vultr":
		sets = []collection{{"/regions", "regions", "regions", nil}, {"/plans", "plans", "plans", nil}, {"/os", "os", "images", nil}, {"/snapshots", "snapshots", "snapshots", nil}}
	default:
		return result, errors.New("unsupported cloud provider")
	}
	for _, set := range sets {
		seen := map[string]bool{}
		rows, limited, err := s.providerRows(ctx, provider, set.path, set.key, set.query)
		if err != nil {
			return result, err
		}
		result.Limited = result.Limited || limited
		for _, row := range rows {
			option := providerOption{ID: providerText(row, "id"), Name: providerText(row, "name"), Architecture: providerText(row, "architecture")}
			if provider.Kind == "digitalocean" {
				option.ID = providerText(row, "slug")
				if option.ID == "" {
					option.ID = providerText(row, "id")
				}
			}
			if option.Name == "" {
				option.Name = option.ID
			}
			if !providerValue.MatchString(option.ID) || len(option.Name) == 0 || seen[option.ID] {
				continue
			}
			seen[option.ID] = true
			var available *bool
			if json.Unmarshal(row["available"], &available) == nil && available != nil && !*available {
				continue
			}
			switch set.target {
			case "regions":
				result.Regions = append(result.Regions, option)
			case "plans":
				switch provider.Kind {
				case "hetzner":
					option.CPUs = int(providerInt(row, "cores"))
					memory, err := strconv.ParseFloat(providerText(row, "memory"), 64)
					if err == nil && memory > 0 && memory < 16384 {
						option.MemoryMB = int64(memory * 1024)
					}
				case "digitalocean":
					option.CPUs = int(providerInt(row, "vcpus"))
					option.MemoryMB = providerInt(row, "memory")
					option.Monthly = providerText(row, "price_monthly")
					option.Currency = "USD"
					_ = json.Unmarshal(row["regions"], &option.Regions)
				case "vultr":
					option.CPUs = int(providerInt(row, "vcpu_count"))
					option.MemoryMB = providerInt(row, "ram")
					option.Monthly = providerText(row, "monthly_cost")
					option.Currency = "USD"
					_ = json.Unmarshal(row["locations"], &option.Regions)
				}
				if option.CPUs <= 0 || option.MemoryMB <= 0 || len(option.Regions) > 512 {
					continue
				}
				result.Plans = append(result.Plans, option)
			case "snapshots":
				if providerText(row, "status") != "complete" {
					continue
				}
				for _, image := range result.Images {
					if image.ID == providerText(row, "os_id") {
						option.ID = "snapshot-" + option.ID
						if !providerValue.MatchString(option.ID) {
							break
						}
						option.Name = providerText(row, "description") + " (" + image.Name + " snapshot)"
						result.Images = append(result.Images, option)
						break
					}
				}
			case "images":
				flavor := strings.ToLower(option.Name + " " + providerText(row, "distribution") + " " + providerText(row, "family") + " " + providerText(row, "os_flavor"))
				supported := false
				for _, name := range []string{"ubuntu", "debian", "alma", "rocky", "centos", "red hat", "rhel"} {
					supported = supported || strings.Contains(flavor, name)
				}
				if supported {
					result.Images = append(result.Images, option)
				}
			}
		}
	}
	if provider.Kind == "vultr" {
		private := result.Images[:0]
		for _, image := range result.Images {
			if strings.HasPrefix(image.ID, "snapshot-") {
				private = append(private, image)
			}
		}
		result.Images = private
	}
	for _, list := range [][]providerOption{result.Regions, result.Plans, result.Images} {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Name == list[j].Name {
				return list[i].ID < list[j].ID
			}
			return list[i].Name < list[j].Name
		})
	}
	return result, nil
}

func providerIdentifier(value string) any {
	if number, err := strconv.ParseInt(value, 10, 64); err == nil && number > 0 {
		return number
	}
	return value
}

func providerCreateBody(provider nodeProvider, name, region, plan, image, userData, marker string) (string, any, error) {
	switch provider.Kind {
	case "hetzner":
		return "/servers", map[string]any{"name": name, "server_type": providerIdentifier(plan), "image": providerIdentifier(image), "location": providerIdentifier(region), "user_data": userData, "labels": map[string]string{"cove-provision": marker}, "public_net": map[string]bool{"enable_ipv4": true, "enable_ipv6": true}}, nil
	case "digitalocean":
		return "/droplets", map[string]any{"name": name, "region": region, "size": plan, "image": providerIdentifier(image), "tags": []string{"cove-provision-" + marker}, "user_data": userData, "ipv6": true, "with_droplet_agent": false}, nil
	case "vultr":
		snapshot, ok := strings.CutPrefix(image, "snapshot-")
		if !ok || !providerValue.MatchString(snapshot) {
			return "", nil, errors.New("select a prepared Vultr Linux snapshot")
		}
		return "/instances", map[string]any{"label": name, "hostname": name, "region": region, "plan": plan, "snapshot_id": snapshot, "tags": []string{"cove-provision-" + marker}, "user_data": base64.StdEncoding.EncodeToString([]byte(userData)), "enable_ipv6": true, "activation_email": false}, nil
	}
	return "", nil, errors.New("unsupported cloud provider")
}

func decodeProviderInstance(provider nodeProvider, row map[string]json.RawMessage, marker string) (providerInstance, error) {
	instance := providerInstance{ID: providerText(row, "id"), Name: providerText(row, "name"), State: providerText(row, "status")}
	if provider.Kind == "vultr" {
		instance.Name = providerText(row, "label")
		instance.Address = providerText(row, "main_ip")
	}
	if !providerValue.MatchString(instance.ID) {
		return instance, errors.New("invalid cloud server identity")
	}
	if provider.Kind == "hetzner" {
		var labels map[string]string
		_ = json.Unmarshal(row["labels"], &labels)
		instance.Owned = labels["cove-provision"] == marker
		var network struct {
			IPv4 struct {
				IP string `json:"ip"`
			} `json:"ipv4"`
		}
		if json.Unmarshal(row["public_net"], &network) == nil {
			instance.Address = network.IPv4.IP
		}
	} else {
		var tags []string
		_ = json.Unmarshal(row["tags"], &tags)
		instance.Owned = slices.Contains(tags, "cove-provision-"+marker)
		if provider.Kind == "digitalocean" {
			var network struct {
				V4 []struct {
					IPAddress string `json:"ip_address"`
					Type      string `json:"type"`
				} `json:"v4"`
			}
			if json.Unmarshal(row["networks"], &network) == nil && len(network.V4) <= 16 {
				for _, address := range network.V4 {
					if address.Type == "public" {
						instance.Address = address.IPAddress
						break
					}
				}
			}
		}
	}
	if instance.Address != "" {
		address, err := netip.ParseAddr(instance.Address)
		if err != nil || !address.Is4() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLinkLocalUnicast() {
			return instance, errors.New("cloud server did not supply a usable public IPv4 address")
		}
	}
	return instance, nil
}
