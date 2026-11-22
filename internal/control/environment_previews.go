package control

import (
	"context"
	"cove/internal/worker"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// previewRecipe configures coordinated environment previews for a project. It is sealed because
// variable overrides may hold credentials.
type previewRecipe struct {
	Enabled         bool                             `json:"enabled"`
	BaseEnvironment string                           `json:"base_environment_id"`
	BranchPattern   string                           `json:"branch_pattern"`
	Repositories    []int64                          `json:"repositories"`
	DomainTemplate  string                           `json:"domain_template"`
	Access          string                           `json:"access"`
	TTLHours        int                              `json:"ttl_hours"`
	MaxPreviews     int                              `json:"max_previews"`
	RequireApproval bool                             `json:"require_approval"`
	Resources       map[string]previewResourcePolicy `json:"resources"`
}

type previewResourcePolicy struct {
	Exclude     bool                       `json:"exclude,omitempty"`
	Database    string                     `json:"database,omitempty"`
	Volumes     map[string]string          `json:"volumes,omitempty"`
	Services    map[string]bool            `json:"services,omitempty"`
	Schedule    bool                       `json:"schedule,omitempty"`
	Variables   map[string]previewVariable `json:"variables,omitempty"`
	FileSecrets string                     `json:"file_secrets,omitempty"`
	External    map[string]string          `json:"external,omitempty"`
	ImageTags   bool                       `json:"image_tags,omitempty"`
}

type previewVariable struct {
	Mode     string `json:"mode"`
	Value    string `json:"value,omitempty"`
	HasValue bool   `json:"has_value,omitempty"`
}

var previewBranchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,98}\*$`)
var previewSlugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

func defaultPreviewRecipe() previewRecipe {
	return previewRecipe{BranchPattern: "staging/*", Access: "private", TTLHours: 72, MaxPreviews: 3, RequireApproval: true, Repositories: []int64{}, Resources: map[string]previewResourcePolicy{}}
}

func validatePreviewRecipe(in previewRecipe) error {
	if !previewBranchPattern.MatchString(in.BranchPattern) || strings.Count(in.BranchPattern, "*") != 1 || strings.Contains(in.BranchPattern, "..") || strings.Contains(in.BranchPattern, "//") {
		return errors.New("branch pattern must be a branch prefix ending in *, for example staging/*")
	}
	if in.Access != "private" && in.Access != "public" {
		return errors.New("preview access must be private or public")
	}
	if in.TTLHours < 1 || in.TTLHours > 720 {
		return errors.New("inactivity cleanup must be 1-720 hours")
	}
	if in.MaxPreviews < 1 || in.MaxPreviews > 20 {
		return errors.New("allow 1-20 previews at a time")
	}
	if len(in.Repositories) > 16 || len(in.Resources) > 64 {
		return errors.New("too many repositories or resources")
	}
	if in.DomainTemplate != "" {
		if !strings.Contains(in.DomainTemplate, "{preview}") || !strings.Contains(in.DomainTemplate, "{resource}") {
			return errors.New("the domain template must contain {resource} and {preview}")
		}
		if _, err := renderPreviewDomain(in.DomainTemplate, "resource", "preview"); err != nil {
			return err
		}
	}
	for _, policy := range in.Resources {
		if policy.Database != "" && policy.Database != "copy" && policy.Database != "empty" {
			return errors.New("database policy must be copy or empty")
		}
		if policy.FileSecrets != "" && policy.FileSecrets != "copy" && policy.FileSecrets != "remove" {
			return errors.New("file secret policy must be copy or remove")
		}
		if len(policy.Volumes) > 64 || len(policy.Services) > 64 || len(policy.Variables) > 256 || len(policy.External) > 64 {
			return errors.New("resource policy is too large")
		}
		for key, mode := range policy.Volumes {
			if len(key) > 300 || (mode != "copy" && mode != "empty") {
				return errors.New("volume policy must be copy or empty")
			}
		}
		for name := range policy.Services {
			if !worker.ValidServiceName(name) {
				return errors.New("invalid service name")
			}
		}
		for key, mode := range policy.External {
			if len(key) > 200 || mode != "omit" {
				return errors.New("dependencies outside the preview can only be omitted")
			}
		}
		for key, variable := range policy.Variables {
			if !variableName.MatchString(key) || len(key) > 128 {
				return errors.New("invalid variable name")
			}
			switch variable.Mode {
			case "copy", "remove":
				if variable.Value != "" {
					return errors.New("only overridden variables have a value")
				}
			case "override":
				if len(variable.Value) > 65536 || strings.ContainsRune(variable.Value, 0) || strings.Contains(variable.Value, "{{") {
					return errors.New("preview overrides must be literal values of at most 64 KiB")
				}
			default:
				return errors.New("variable policy must be copy, override or remove")
			}
		}
	}
	return nil
}

// previewSlug turns a branch suffix into a DNS label part. Truncated, empty or colliding suffixes
// get a short hash so two branches can never share a preview hostname.
func previewSlug(suffix string, taken func(string) bool) string {
	slug := strings.Trim(previewSlugUnsafe.ReplaceAllString(strings.ToLower(suffix), "-"), "-")
	sum := sha256.Sum256([]byte(suffix))
	hash := hex.EncodeToString(sum[:3])
	if len(slug) > 24 {
		slug = strings.Trim(slug[:24], "-") + "-" + hash
	}
	if slug == "" {
		slug = "p-" + hash
	}
	if taken(slug) {
		slug = strings.Trim(slug, "-")
		if len(slug) > 24 {
			slug = slug[:24]
		}
		slug = strings.Trim(slug, "-") + "-" + hash
	}
	return slug
}

func resourceSlug(name string) string {
	slug := strings.Trim(previewSlugUnsafe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(slug) > 24 {
		slug = strings.Trim(slug[:24], "-")
	}
	if slug == "" {
		slug = "app"
	}
	return slug
}

func renderPreviewDomain(template, resource, preview string) (string, error) {
	domain := strings.ToLower(strings.NewReplacer("{resource}", resource, "{preview}", preview).Replace(template))
	if err := worker.ValidateRoutes([]worker.Route{{Domain: domain, Service: "app", Port: 80}}, "service"); err != nil {
		return "", errors.New("the domain template must produce lowercase hostnames such as {resource}-{preview}.preview.example.com")
	}
	return domain, nil
}

func (s *Server) loadPreviewRecipe(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, project string) (previewRecipe, int64, bool, error) {
	recipe := defaultPreviewRecipe()
	var encrypted []byte
	var version int64
	err := q.QueryRow(ctx, `SELECT configuration,version FROM preview_recipes WHERE project_id=$1`, project).Scan(&encrypted, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return recipe, 0, false, nil
	}
	if err != nil {
		return recipe, 0, false, err
	}
	raw, err := unseal(s.Key, encrypted)
	if err != nil {
		return recipe, 0, false, err
	}
	if err = json.Unmarshal(raw, &recipe); err != nil {
		return recipe, 0, false, err
	}
	if recipe.Resources == nil {
		recipe.Resources = map[string]previewResourcePolicy{}
	}
	return recipe, version, true, nil
}

// redactPreviewRecipe removes override values before a recipe leaves the server.
func redactPreviewRecipe(recipe previewRecipe) previewRecipe {
	resources := map[string]previewResourcePolicy{}
	for id, policy := range recipe.Resources {
		variables := map[string]previewVariable{}
		for key, variable := range policy.Variables {
			variables[key] = previewVariable{Mode: variable.Mode, HasValue: variable.Value != ""}
		}
		policy.Variables = variables
		resources[id] = policy
	}
	recipe.Resources = resources
	return recipe
}

// previewSource is a production resource in the recipe's base environment together with what the
// plan needs to know about it.
type previewSource struct {
	ID, Name, Kind, Node, Project, Team string
	Config                              workloadInput
	Capable                             bool
	NodeReady                           bool
	Repository                          int64
	RepositoryName                      string
	Installation                        int64
	InstallationAssigned                bool
	Release                             string
	ReleaseAction                       string
	Deployed                            *worker.Request
	DeployedRevision                    string
	DeployedAt                          *time.Time
	JobCron                             string
	Inventory                           *worker.PreviewInventory
	InventoryError                      string
}

type previewDependency struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	Inside   bool   `json:"inside"`
	TargetID string `json:"target_id,omitempty"`
	SourceID string `json:"source_id,omitempty"`
}

type previewBlocker struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Resolution string `json:"resolution"`
	Key        string `json:"key,omitempty"`
}

type previewVolumePlan struct {
	Key      string   `json:"key"`
	Type     string   `json:"type"`
	Source   string   `json:"source"`
	Services []string `json:"services"`
	Mode     string   `json:"mode"`
	Default  string   `json:"default"`
	Allowed  []string `json:"allowed"`
	Reason   string   `json:"reason,omitempty"`
}

type previewServicePlan struct {
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	Tags    []string `json:"tags,omitempty"`
	Run     bool     `json:"run"`
	Default bool     `json:"default"`
	Reason  string   `json:"reason"`
}

type previewVariablePlan struct {
	Key            string `json:"key"`
	Classification string `json:"classification"`
	Mode           string `json:"mode"`
	HasOverride    bool   `json:"has_override"`
	Reason         string `json:"reason"`
}

type previewResourcePlan struct {
	ID          string                `json:"id"`
	Name        string                `json:"name"`
	Kind        string                `json:"kind"`
	Included    bool                  `json:"included"`
	Repository  string                `json:"repository,omitempty"`
	RepoID      int64                 `json:"repository_id,omitempty"`
	FollowsRepo bool                  `json:"follows_branch"`
	SourceKind  string                `json:"source_kind"`
	Revision    string                `json:"deployed_revision,omitempty"`
	DeployedAt  *time.Time            `json:"deployed_at,omitempty"`
	Database    *previewDatabasePlan  `json:"database,omitempty"`
	Services    []previewServicePlan  `json:"services"`
	Volumes     []previewVolumePlan   `json:"volumes"`
	Variables   []previewVariablePlan `json:"variables"`
	Depends     []previewDependency   `json:"dependencies"`
	Routes      []string              `json:"routes"`
	Blockers    []previewBlocker      `json:"blockers"`
	Notes       []string              `json:"notes"`
	Schedule    string                `json:"schedule,omitempty"`
	FileSecrets int                   `json:"file_secrets,omitempty"`
}

type previewDatabasePlan struct {
	Engine    string `json:"engine"`
	Service   string `json:"service"`
	Name      string `json:"name"`
	Mode      string `json:"mode"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
}

type previewPlan struct {
	Resources    []previewResourcePlan `json:"resources"`
	Repositories []previewRepository   `json:"repositories"`
	Blocked      bool                  `json:"blocked"`
	Hostnames    []string              `json:"example_hostnames"`
	DNS          string                `json:"dns"`
}

type previewRepository struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Selected bool   `json:"selected"`
	Assigned bool   `json:"assigned"`
}

var sensitiveVariable = regexp.MustCompile(`(?i)(SECRET|TOKEN|PASSWORD|PASSWD|_PASS$|^PASS$|PRIVATE|CREDENTIAL|API_?KEY|ACCESS_KEY|_KEY$|^KEY$|DSN|_URL$|_URI$|^URL$|^URI$|HOST$|ENDPOINT|WEBHOOK|SMTP|MAIL|STRIPE|TWILIO|SENDGRID|SENTRY|AWS_|^S3_|OAUTH|CLIENT_ID|SLACK|DISCORD|PAYPAL|BUCKET)`)
var urlHost = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://(?:[^@/\s]*@)?(\[[^\]]+\]|[^:/?#\s,;]+)`)
var hostPort = regexp.MustCompile(`(?i)(?:^|[\s,;=@])([a-z0-9][a-z0-9.-]*):\d{2,5}\b`)
var databaseImage = regexp.MustCompile(`(?i)(^|/)(postgres|postgis|timescale\w*|mysql|mariadb|percona|mongo|mongodb|clickhouse|elasticsearch|opensearch|cassandra|scylla|influxdb|couchdb|neo4j|cockroach\w*|questdb|surrealdb|rethinkdb)[:@/]`)
var cacheImage = regexp.MustCompile(`(?i)(^|/)(redis|valkey|keydb|memcached|dragonfly)[:@/]`)

func imageMatches(pattern *regexp.Regexp, service worker.PreviewInventoryService) bool {
	for _, tag := range append([]string{service.Image}, service.Tags...) {
		if pattern.MatchString(tag + ":") {
			return true
		}
	}
	return false
}

func valueHosts(value string) []string {
	hosts := []string{}
	for _, match := range urlHost.FindAllStringSubmatch(value, -1) {
		hosts = append(hosts, strings.ToLower(strings.Trim(match[1], "[]")))
	}
	if len(hosts) == 0 {
		for _, match := range hostPort.FindAllStringSubmatch(value, -1) {
			hosts = append(hosts, strings.ToLower(match[1]))
		}
	}
	return hosts
}

// loadPreviewSources reads the production resources of an environment. Inventories are fetched
// from the workers concurrently and never change those resources.
func (s *Server) loadPreviewSources(ctx context.Context, team, environment string, inventory bool) ([]*previewSource, error) {
	rows, err := s.DB.Query(ctx, `SELECT w.id,w.name,w.kind,w.node_id,w.project_id,w.configuration,w.job_cron,
 COALESCE((n.capabilities->>'environment_previews')::boolean,false),n.state='ready',
 COALESCE(g.repository_id,0),COALESCE(g.repository,''),COALESCE(g.installation_id,0),
 EXISTS(SELECT 1 FROM github_installation_teams i WHERE i.installation_id=g.installation_id AND i.team_id=w.team_id),
 COALESCE(o.id,''),COALESCE(o.action,''),o.payload,COALESCE(o.result->>'revision',''),o.updated_at
 FROM workloads w JOIN nodes n ON n.id=w.node_id LEFT JOIN github_sources g ON g.workload_id=w.id
 LEFT JOIN LATERAL (SELECT id,action,payload,result,updated_at FROM operations WHERE workload_id=w.id AND action IN ('deploy','rollback') AND state='succeeded' ORDER BY updated_at DESC LIMIT 1) o ON true
 WHERE w.team_id=$1 AND w.environment_id=$2 AND NOT EXISTS(SELECT 1 FROM environment_preview_resources r WHERE r.resource_id=w.id)
 ORDER BY w.name,w.id`, team, environment)
	if err != nil {
		return nil, err
	}
	sources := []*previewSource{}
	for rows.Next() {
		var src previewSource
		var config, payload []byte
		var updated *time.Time
		if err = rows.Scan(&src.ID, &src.Name, &src.Kind, &src.Node, &src.Project, &config, &src.JobCron, &src.Capable, &src.NodeReady, &src.Repository, &src.RepositoryName, &src.Installation, &src.InstallationAssigned, &src.Release, &src.ReleaseAction, &payload, &src.DeployedRevision, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		raw, err := unseal(s.Key, config)
		if err != nil || json.Unmarshal(raw, &src.Config) != nil {
			rows.Close()
			return nil, errors.New("resource configuration unavailable")
		}
		src.Team = team
		src.DeployedAt = updated
		if len(payload) > 0 {
			raw, err = unseal(s.Key, payload)
			var request worker.Request
			if err != nil || json.Unmarshal(raw, &request) != nil {
				rows.Close()
				return nil, errors.New("deployed operation unavailable")
			}
			src.Deployed = &request
		}
		sources = append(sources, &src)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// A rollback re-runs an earlier release; its source and Compose come from that release's deployment.
	for _, src := range sources {
		for depth := 0; depth < 20 && src.Deployed != nil && src.ReleaseAction == "rollback"; depth++ {
			var payload []byte
			var action string
			err = s.DB.QueryRow(ctx, `SELECT action,payload FROM operations WHERE id=$1 AND workload_id=$2 AND state='succeeded'`, src.Deployed.Release, src.ID).Scan(&action, &payload)
			if err != nil {
				src.Deployed = nil
				break
			}
			raw, err := unseal(s.Key, payload)
			var request worker.Request
			if err != nil || json.Unmarshal(raw, &request) != nil {
				src.Deployed = nil
				break
			}
			src.Deployed = &request
			src.ReleaseAction = action
		}
	}
	if !inventory {
		return sources, nil
	}
	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for _, src := range sources {
		if src.Deployed == nil {
			continue
		}
		wg.Go(func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			call, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			request := worker.RPCRequest{Method: "environment_preview_inventory", ID: src.ID}
			if src.Config.Backup != nil && src.Config.Backup.Database != nil {
				request.Request = &worker.Request{Backup: &worker.BackupConfig{Database: src.Config.Backup.Database}}
			}
			response, err := s.nodeRPC(call, src.Node, request)
			if err != nil || response.Inventory == nil {
				src.InventoryError = "the worker could not describe the deployed release"
				var remote *RemoteError
				if errors.As(err, &remote) {
					src.InventoryError = remote.Error()
				}
				return
			}
			src.Inventory = response.Inventory
		})
	}
	wg.Wait()
	return sources, nil
}

type previewTopology struct {
	connections []struct {
		id, source, sourceService, target, targetService, alias string
		port                                                    int
	}
	networks []struct {
		network, workload, service, alias, name string
	}
	bindings []struct {
		workload, name, source, sourceKey string
	}
}

func (s *Server) loadPreviewTopology(ctx context.Context, team string, ids []string) (*previewTopology, error) {
	topology := &previewTopology{}
	rows, err := s.DB.Query(ctx, `SELECT id,source_id,source_service,target_id,target_service,alias,port FROM resource_connections WHERE team_id=$1 AND NOT removing AND (source_id=ANY($2) OR target_id=ANY($2)) ORDER BY id`, team, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c struct {
			id, source, sourceService, target, targetService, alias string
			port                                                    int
		}
		if err = rows.Scan(&c.id, &c.source, &c.sourceService, &c.target, &c.targetService, &c.alias, &c.port); err != nil {
			rows.Close()
			return nil, err
		}
		topology.connections = append(topology.connections, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.DB.Query(ctx, `SELECT m.network_id,m.workload_id,m.service,m.alias,n.name FROM workload_networks m JOIN shared_networks n ON n.id=m.network_id WHERE n.team_id=$1 AND m.network_id IN (SELECT network_id FROM workload_networks WHERE workload_id=ANY($2)) ORDER BY m.network_id,m.workload_id,m.service`, team, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n struct{ network, workload, service, alias, name string }
		if err = rows.Scan(&n.network, &n.workload, &n.service, &n.alias, &n.name); err != nil {
			rows.Close()
			return nil, err
		}
		topology.networks = append(topology.networks, n)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.DB.Query(ctx, `SELECT workload_id,name,source_id,source_key FROM resource_variable_bindings WHERE workload_id=ANY($1) ORDER BY workload_id,name`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b struct{ workload, name, source, sourceKey string }
		if err = rows.Scan(&b.workload, &b.name, &b.source, &b.sourceKey); err != nil {
			rows.Close()
			return nil, err
		}
		topology.bindings = append(topology.bindings, b)
	}
	rows.Close()
	return topology, rows.Err()
}

// planPreview evaluates the recipe against the current production environment. The same plan is
// shown in the recipe editor and enforced when a preview is created.
func (s *Server) planPreview(ctx context.Context, team string, recipe previewRecipe, sources []*previewSource) (*previewPlan, error) {
	ids := []string{}
	for _, src := range sources {
		ids = append(ids, src.ID)
	}
	topology, err := s.loadPreviewTopology(ctx, team, ids)
	if err != nil {
		return nil, err
	}
	included := map[string]bool{}
	for _, src := range sources {
		included[src.ID] = !recipe.Resources[src.ID].Exclude
	}
	selected := map[int64]bool{}
	for _, id := range recipe.Repositories {
		selected[id] = true
	}
	plan := &previewPlan{Resources: []previewResourcePlan{}, Repositories: []previewRepository{}, Hostnames: []string{}}
	seenRepositories := map[int64]bool{}
	for _, src := range sources {
		if src.Repository != 0 && !seenRepositories[src.Repository] {
			seenRepositories[src.Repository] = true
			plan.Repositories = append(plan.Repositories, previewRepository{ID: src.Repository, Name: repositoryName(src.RepositoryName), Selected: selected[src.Repository], Assigned: src.InstallationAssigned})
		}
	}
	for id := range selected {
		if !seenRepositories[id] {
			plan.Repositories = append(plan.Repositories, previewRepository{ID: id, Name: fmt.Sprintf("Repository %d (no longer used here)", id), Selected: true})
		}
	}
	sort.Slice(plan.Repositories, func(i, j int) bool { return plan.Repositories[i].Name < plan.Repositories[j].Name })
	for _, src := range sources {
		item := s.planPreviewResource(recipe, src, topology, included, selected)
		if item.Included && len(item.Blockers) > 0 {
			plan.Blocked = true
		}
		plan.Resources = append(plan.Resources, item)
	}
	if recipe.DomainTemplate != "" {
		for _, item := range plan.Resources {
			if !item.Included || len(item.Routes) == 0 {
				continue
			}
			plan.Hostnames = append(plan.Hostnames, item.Routes...)
		}
		plan.DNS = "manual"
		domain := ""
		if len(plan.Hostnames) > 0 {
			domain = plan.Hostnames[0]
		}
		if domain != "" {
			automated, err := s.previewDNSAutomated(ctx, team, domain)
			if err != nil {
				return nil, err
			}
			if automated {
				plan.DNS = "cloudflare"
			}
		}
	}
	return plan, nil
}

func repositoryName(cloneURL string) string {
	name := strings.TrimSuffix(strings.TrimPrefix(cloneURL, "https://github.com/"), ".git")
	if name == "" {
		return cloneURL
	}
	return name
}

func (s *Server) planPreviewResource(recipe previewRecipe, src *previewSource, topology *previewTopology, included map[string]bool, selected map[int64]bool) previewResourcePlan {
	policy := recipe.Resources[src.ID]
	item := previewResourcePlan{ID: src.ID, Name: src.Name, Kind: src.Kind, Included: !policy.Exclude, Services: []previewServicePlan{}, Volumes: []previewVolumePlan{}, Variables: []previewVariablePlan{}, Depends: []previewDependency{}, Routes: []string{}, Blockers: []previewBlocker{}, Notes: []string{}, Schedule: src.JobCron, FileSecrets: len(src.Config.FileSecrets)}
	block := func(code, message, resolution, key string) {
		item.Blockers = append(item.Blockers, previewBlocker{Code: code, Message: message, Resolution: resolution, Key: key})
	}
	item.SourceKind = "compose"
	if src.Config.Source != nil && !src.Config.Source.InlineCompose {
		item.SourceKind = "git"
	}
	if src.Repository != 0 {
		item.Repository = repositoryName(src.RepositoryName)
		item.RepoID = src.Repository
		item.FollowsRepo = selected[src.Repository]
	}
	item.Revision = src.DeployedRevision
	item.DeployedAt = src.DeployedAt
	inside := func(id string) bool { return included[id] }
	internal := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	routedServices := map[string]bool{}
	for i, route := range src.Config.Routes {
		routedServices[route.Service] = true
		if recipe.DomainTemplate != "" {
			name := resourceSlug(src.Name)
			if i > 0 {
				name = fmt.Sprintf("%s-%d", name, i+1)
			}
			if domain, err := renderPreviewDomain(recipe.DomainTemplate, name, "example"); err == nil {
				item.Routes = append(item.Routes, domain)
			}
		}
	}
	inbound := map[string]bool{}
	for _, c := range topology.connections {
		label := fmt.Sprintf("%s:%d", c.alias, c.port)
		switch {
		case c.source == src.ID:
			internal[strings.ToLower(c.alias)] = inside(c.target)
			dependency := previewDependency{Key: "connection:" + c.id, Kind: "connection", Label: label, Inside: inside(c.target), TargetID: c.target}
			item.Depends = append(item.Depends, dependency)
			if !dependency.Inside && policy.External[dependency.Key] != "omit" {
				block("external_connection", fmt.Sprintf("Private connection %s reaches a resource outside this preview.", label), "Omit it; the preview never connects to production resources.", dependency.Key)
			}
		case c.target == src.ID:
			if inside(c.source) {
				inbound[c.targetService] = true
			}
		}
	}
	networkPeers := map[string][]string{}
	for _, n := range topology.networks {
		networkPeers[n.network] = append(networkPeers[n.network], n.workload)
	}
	for _, n := range topology.networks {
		if n.workload != src.ID {
			continue
		}
		outside := false
		for _, peer := range topology.networks {
			if peer.network == n.network && peer.workload != src.ID {
				if inside(peer.workload) {
					internal[strings.ToLower(peer.alias)] = true
					inbound[n.service] = true
				} else {
					outside = true
				}
			}
		}
		key := "shared:" + n.network
		item.Depends = append(item.Depends, previewDependency{Key: key, Kind: "network", Label: n.name, Inside: !outside})
		if outside && policy.External[key] != "omit" {
			block("external_network_member", fmt.Sprintf("Shared network %s includes resources outside this preview.", n.name), "Omit them; the preview gets its own copy of the network with only preview resources.", key)
		}
	}
	bindingSource := map[string]string{}
	for _, b := range topology.bindings {
		if b.workload != src.ID {
			continue
		}
		bindingSource[b.name] = b.source
		item.Depends = append(item.Depends, previewDependency{Key: "binding:" + b.name, Kind: "variable", Label: b.name, Inside: inside(b.source), SourceID: b.source})
	}
	if src.Config.Private != nil && policy.External["tailscale"] != "omit" {
		block("tailscale", "This resource joins a Tailscale network.", "Omit Tailscale access; previews never join production tailnets.", "tailscale")
	}
	if !src.NodeReady {
		block("node_unavailable", "The resource's node is not connected.", "Previews wait for the node to reconnect.", "")
	} else if !src.Capable {
		block("worker_capability", "The resource's node cannot run environment previews.", "Upgrade and verify the worker on this node.", "")
	}
	if src.Repository != 0 && selected[src.Repository] && !src.InstallationAssigned {
		block("repository_unavailable", "The GitHub App installation for this repository is not shared with this team.", "Share the installation with the team in Integrations.", "")
	}
	if src.Deployed == nil {
		block("not_deployed", "This resource has no successful deployment to copy.", "Deploy it first or exclude it from previews.", "")
	} else if src.Inventory == nil && src.InventoryError != "" && src.NodeReady {
		block("inventory_unavailable", "Could not read the deployed release: "+src.InventoryError, "Check the node and try again.", "")
	}
	if src.Kind == "job" && src.Config.Source == nil && src.Inventory != nil && !policy.ImageTags {
		for _, service := range src.Inventory.Services {
			if !strings.Contains(service.Image, "@sha256:") {
				block("unpinned_image", fmt.Sprintf("Job image %s uses a tag, so a preview may run a different image than production last ran.", service.Image), "Accept tag resolution or exclude the job.", "image_tags")
				break
			}
		}
	}
	if len(src.Config.FileSecrets) > 0 && policy.FileSecrets == "" {
		block("file_secrets", fmt.Sprintf("%d secret files are mounted into this resource.", len(src.Config.FileSecrets)), "Copy or remove them for previews.", "file_secrets")
	}
	var database *worker.DatabaseBackup
	if src.Config.Backup != nil {
		database = src.Config.Backup.Database
	}
	if inv := src.Inventory; inv != nil {
		for _, name := range inv.ExternalNetworks {
			key := "network:" + name
			item.Depends = append(item.Depends, previewDependency{Key: key, Kind: "external_network", Label: name})
			if policy.External[key] != "omit" {
				block("external_network", fmt.Sprintf("Compose attaches to the external network %s.", name), "Omit it; previews never join networks shared with production.", key)
			}
		}
		dependents := map[string]bool{}
		for _, service := range inv.Services {
			for _, dependency := range service.DependsOn {
				dependents[dependency] = true
			}
		}
		for _, service := range inv.Services {
			internal[strings.ToLower(service.Name)] = true
			if len(service.Unsafe) > 0 {
				block("unsafe_option", fmt.Sprintf("Service %s uses %s.", service.Name, strings.Join(service.Unsafe, ", ")), "Previews cannot run with host access. Exclude this resource.", "")
			}
			if service.Ports {
				item.Notes = append(item.Notes, fmt.Sprintf("Service %s publishes host ports in production; previews do not publish host ports.", service.Name))
			}
			run, reason := false, "No domain, private connection or dependent service uses it, so it is treated as a background service."
			switch {
			case routedServices[service.Name]:
				run, reason = true, "Serves a domain."
			case inbound[service.Name]:
				run, reason = true, "Other preview resources connect to it."
			case dependents[service.Name]:
				run, reason = true, "Another service in this resource depends on it."
			case database != nil && database.Service == service.Name:
				run, reason = true, "Database service."
			}
			plan := previewServicePlan{Name: service.Name, Image: service.Image, Tags: service.Tags, Default: run, Run: run, Reason: reason}
			if value, ok := policy.Services[service.Name]; ok {
				plan.Run = value
			}
			if src.Kind == "job" {
				plan.Default, plan.Run, plan.Reason = false, policy.Schedule, "Jobs only run on their schedule when it is enabled for previews."
			}
			item.Services = append(item.Services, plan)
		}
		databaseServices := map[string]bool{}
		if database != nil {
			databaseServices[database.Service] = true
			mode := policy.Database
			if mode == "" {
				mode = "copy"
			}
			dbPlan := &previewDatabasePlan{Engine: database.Engine, Service: database.Service, Name: database.Name, Mode: mode}
			if inv.Database != nil {
				dbPlan.Supported, dbPlan.Reason, dbPlan.Bytes = inv.Database.Supported, inv.Database.Reason, inv.Database.Bytes
			}
			if mode == "copy" && !dbPlan.Supported {
				block("database_unsupported", "The production database cannot be copied: "+dbPlan.Reason, "Fix the database or start the preview with an empty database.", "database")
			}
			item.Database = dbPlan
		}
		servicesByName := map[string]worker.PreviewInventoryService{}
		for _, service := range inv.Services {
			servicesByName[service.Name] = service
		}
		for _, volume := range inv.Volumes {
			v := previewVolumePlan{Key: volume.Key, Type: volume.Type, Source: volume.Source, Services: volume.Services, Allowed: []string{"copy", "empty"}, Default: "copy"}
			isDatabase, isCache, otherDatabase := false, false, false
			for _, name := range volume.Services {
				if databaseServices[name] {
					isDatabase = true
				}
				if imageMatches(cacheImage, servicesByName[name]) {
					isCache = true
				}
				if !databaseServices[name] && imageMatches(databaseImage, servicesByName[name]) {
					otherDatabase = true
				}
			}
			switch {
			case isDatabase:
				v.Allowed = []string{"database", "empty"}
				v.Default = "database"
				if item.Database != nil && item.Database.Mode == "empty" {
					v.Default = "empty"
				}
				v.Mode = v.Default
				v.Reason = "Restored from a fresh logical snapshot of the production database."
				if v.Mode == "empty" {
					v.Reason = "Starts empty; the application must create its schema."
				}
				item.Volumes = append(item.Volumes, v)
				continue
			case volume.File:
				v.Allowed = []string{}
				v.Reason = "A host file is mounted here."
				block("host_file", fmt.Sprintf("Host file %s is mounted; previews cannot copy single host files.", volume.Source), "Move the file into the repository or a volume, or exclude the resource.", volume.Key)
			case otherDatabase:
				v.Allowed = []string{"empty"}
				v.Default = ""
				v.Reason = "A live database cannot be copied file by file. Configure a logical PostgreSQL or MariaDB backup on the production resource to copy its data."
			case isCache:
				v.Default = "empty"
				v.Reason = "Cache data starts empty."
			case volume.Missing:
				v.Allowed = []string{"empty"}
				v.Default = ""
				v.Reason = "The production storage does not exist on the node."
			default:
				v.Reason = "Copied file by file from read-only production storage when the preview is created."
			}
			v.Mode = v.Default
			if chosen := policy.Volumes[volume.Key]; chosen != "" {
				allowed := false
				for _, option := range v.Allowed {
					allowed = allowed || option == chosen
				}
				if allowed {
					v.Mode = chosen
				}
			}
			if v.Mode == "" && !volume.File {
				block("volume_policy", fmt.Sprintf("Choose how %s starts in previews.", volume.Source), v.Reason, volume.Key)
			}
			item.Volumes = append(item.Volumes, v)
		}
	}
	keys := make([]string, 0, len(src.Config.Environment))
	for key := range src.Config.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := src.Config.Environment[key]
		chosen, decided := policy.Variables[key]
		variable := previewVariablePlan{Key: key, Classification: "plain", Mode: "copy", Reason: "Copied unchanged."}
		hosts := valueHosts(value)
		allInternal := len(hosts) > 0
		for _, host := range hosts {
			allInternal = allInternal && internal[host]
		}
		external := false
		for _, match := range sharedReference.FindAllStringSubmatch(value, -1) {
			if match[1] != "resource" {
				external = true
				continue
			}
			if match[1] == "resource" {
				if source, ok := bindingSource[match[2]]; ok && inside(source) {
					allInternal = true
				} else {
					external = true
				}
			}
		}
		switch {
		case external || strings.Contains(value, "{{secret."):
			variable.Classification, variable.Mode, variable.Reason = "sensitive", "", "Uses a shared variable, secret provider or resource outside this preview."
		case allInternal:
			variable.Classification, variable.Reason = "internal", "Points at a service inside the preview, which resolves to the preview's own copy."
		case sensitiveVariable.MatchString(key):
			variable.Classification, variable.Mode, variable.Reason = "sensitive", "", "Looks like a credential or external endpoint. Decide whether the preview may use the production value."
		}
		if decided {
			variable.Mode = chosen.Mode
			variable.HasOverride = chosen.Value != ""
		}
		if variable.Mode == "" {
			block("variable_decision", fmt.Sprintf("Decide how previews use %s.", key), "Copy the production value, override it, or remove it.", key)
		}
		item.Variables = append(item.Variables, variable)
	}
	if policy.Exclude {
		item.Blockers = []previewBlocker{}
	}
	for _, dependency := range item.Depends {
		if dependency.Kind == "variable" && !dependency.Inside {
			if _, ok := policy.Variables[dependency.Label]; !ok && !policy.Exclude {
				exists := false
				for _, b := range item.Blockers {
					exists = exists || (b.Code == "variable_decision" && b.Key == dependency.Label)
				}
				if !exists {
					block("variable_decision", fmt.Sprintf("%s takes its value from a resource outside this preview.", dependency.Label), "Copy the production value, override it, or remove it.", dependency.Label)
				}
			}
		}
	}
	return item
}

func (s *Server) previewDNSAutomated(ctx context.Context, team, domain string) (bool, error) {
	rows, err := s.DB.Query(ctx, `SELECT z.configuration,t.domains FROM cloudflare_zones z JOIN cloudflare_zone_teams t ON t.zone_id=z.zone_id WHERE t.team_id=$1 AND ($2=z.domain OR $2 LIKE '%.'||z.domain)`, team, domain)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var domains []string
		if err = rows.Scan(&raw, &domains); err != nil {
			return false, err
		}
		data, err := unseal(s.Key, raw)
		var zone cloudflareZone
		if err != nil || json.Unmarshal(data, &zone) != nil {
			return false, errors.New("DNS configuration unavailable")
		}
		if zone.Enabled && cloudflareDomainAllowed(domains, domain) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// previewDomainAllowed refuses hostnames inside a Cloudflare zone registered in Cove that is not
// shared with the team, so a team cannot claim another team's domains for previews.
func (s *Server) previewDomainAllowed(ctx context.Context, team, domain string) (bool, error) {
	rows, err := s.DB.Query(ctx, `SELECT z.zone_id,COALESCE(t.domains,ARRAY[]::text[]) FROM cloudflare_zones z LEFT JOIN cloudflare_zone_teams t ON t.zone_id=z.zone_id AND t.team_id=$1 WHERE $2=z.domain OR $2 LIKE '%.'||z.domain`, team, domain)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var zone string
		var domains []string
		if err = rows.Scan(&zone, &domains); err != nil {
			return false, err
		}
		if !cloudflareDomainAllowed(domains, domain) {
			return false, nil
		}
	}
	return true, rows.Err()
}

func (s *Server) previewRecipeHandler(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("id")
	team := actor(r).Team
	if r.Method == "GET" {
		recipe, _, saved, err := s.loadPreviewRecipe(r.Context(), s.DB, project)
		if err != nil {
			http.Error(w, "preview recipe unavailable", 503)
			return
		}
		environment := r.URL.Query().Get("environment")
		if environment == "" {
			environment = recipe.BaseEnvironment
		}
		response := map[string]any{"recipe": redactPreviewRecipe(recipe), "saved": saved, "plan": nil}
		if environment != "" {
			var found bool
			if err = s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM environments e WHERE e.id=$1 AND e.project_id=$2 AND e.team_id=$3 AND NOT EXISTS(SELECT 1 FROM environment_previews p WHERE p.environment_id=e.id))`, environment, project, team).Scan(&found); err != nil {
				http.Error(w, "environment unavailable", 503)
				return
			}
			if !found {
				http.Error(w, "select a production environment of this project", 400)
				return
			}
			recipe.BaseEnvironment = environment
			sources, err := s.loadPreviewSources(r.Context(), team, environment, true)
			if err != nil {
				http.Error(w, "environment resources unavailable", 503)
				return
			}
			plan, err := s.planPreview(r.Context(), team, recipe, sources)
			if err != nil {
				http.Error(w, "preview plan unavailable", 503)
				return
			}
			response["plan"] = plan
		}
		respond(w, 200, response)
		return
	}
	var in previewRecipe
	if !decode(w, r, &in) {
		return
	}
	if in.Resources == nil {
		in.Resources = map[string]previewResourcePolicy{}
	}
	if in.Repositories == nil {
		in.Repositories = []int64{}
	}
	if err := validatePreviewRecipe(in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var found bool
	if err := s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM environments e WHERE e.id=$1 AND e.project_id=$2 AND e.team_id=$3 AND NOT EXISTS(SELECT 1 FROM environment_previews p WHERE p.environment_id=e.id))`, in.BaseEnvironment, project, team).Scan(&found); err != nil {
		http.Error(w, "environment unavailable", 503)
		return
	}
	if !found {
		http.Error(w, "select a production environment of this project", 400)
		return
	}
	if !s.requireAccess(w, r, "environment", in.BaseEnvironment, 3) {
		return
	}
	for _, id := range in.Repositories {
		var used bool
		if err := s.DB.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM github_sources g JOIN workloads w ON w.id=g.workload_id JOIN github_installation_teams i ON i.installation_id=g.installation_id AND i.team_id=w.team_id WHERE g.repository_id=$1 AND w.environment_id=$2 AND w.team_id=$3)`, id, in.BaseEnvironment, team).Scan(&used); err != nil {
			http.Error(w, "repositories unavailable", 503)
			return
		}
		if !used {
			http.Error(w, "select repositories connected to resources in the production environment through a GitHub App installation shared with this team", 400)
			return
		}
	}
	if in.DomainTemplate != "" {
		domain, _ := renderPreviewDomain(in.DomainTemplate, "app", "preview")
		allowed, err := s.previewDomainAllowed(r.Context(), team, domain)
		if err != nil {
			http.Error(w, "domains unavailable", 503)
			return
		}
		if !allowed {
			http.Error(w, "the domain template is inside a Cloudflare zone that is not shared with this team", 403)
			return
		}
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended('cove-preview-recipe:'||$1,0))`, project); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	old, _, _, err := s.loadPreviewRecipe(r.Context(), tx, project)
	if err != nil {
		http.Error(w, "preview recipe unavailable", 503)
		return
	}
	// Override values are write-only; an override sent without a value keeps the saved one.
	for id, policy := range in.Resources {
		for key, variable := range policy.Variables {
			if variable.Mode == "override" && variable.Value == "" {
				previous := old.Resources[id].Variables[key]
				if previous.Mode != "override" || previous.Value == "" {
					http.Error(w, fmt.Sprintf("enter a preview value for %s", key), 400)
					return
				}
				variable.Value = previous.Value
			}
			variable.HasValue = false
			policy.Variables[key] = variable
		}
		in.Resources[id] = policy
	}
	raw, err := json.Marshal(in)
	if err != nil {
		http.Error(w, "invalid recipe", 400)
		return
	}
	encrypted, err := seal(s.Key, raw)
	if err != nil {
		http.Error(w, "could not protect recipe", 500)
		return
	}
	if _, err = tx.Exec(r.Context(), `INSERT INTO preview_recipes(project_id,team_id,base_environment_id,enabled,configuration) VALUES($1,$2,$3,$4,$5) ON CONFLICT(project_id) DO UPDATE SET base_environment_id=EXCLUDED.base_environment_id,enabled=EXCLUDED.enabled,configuration=EXCLUDED.configuration,version=preview_recipes.version+1,updated_at=now(),next_scan_at=now()`, project, team, in.BaseEnvironment, in.Enabled, encrypted); err != nil {
		http.Error(w, "could not save recipe", 503)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not save recipe", 503)
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
