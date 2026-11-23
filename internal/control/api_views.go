package control

import (
	"cove/internal/worker"
	"encoding/json"
	"time"
)

type workloadView struct {
	EnvironmentPaused    bool                            `json:"environment_paused"`
	Rollout              *worker.RolloutOptions          `json:"rollout"`
	PendingAdoptionID    string                          `json:"pending_adoption_id,omitempty"`
	ID                   string                          `json:"id"`
	Name                 string                          `json:"name"`
	NodeID               string                          `json:"node_id"`
	ProjectID            string                          `json:"project_id"`
	EnvironmentID        string                          `json:"environment_id"`
	Kind                 string                          `json:"kind"`
	JobService           string                          `json:"job_service"`
	Compose              string                          `json:"compose"`
	Source               *worker.GitSource               `json:"source"`
	HasSourceToken       bool                            `json:"has_source_token"`
	EnvironmentKeys      []string                        `json:"environment_keys"`
	Routes               []worker.Route                  `json:"routes"`
	GuardedDeploy        *bool                           `json:"guarded_deploy"`
	FileSecrets          []worker.FileSecret             `json:"file_secrets"`
	Hooks                *worker.DeploymentHooks         `json:"hooks"`
	Limits               map[string]worker.ServiceLimits `json:"limits"`
	Deployed             bool                            `json:"deployed"`
	Permissions          []string                        `json:"permissions"`
	ManuallyStopped      bool                            `json:"manually_stopped"`
	PreviewSourceID      string                          `json:"preview_source_id"`
	EnvironmentPreviewID string                          `json:"environment_preview_id"`
}
type operationView struct {
	Reason                  string          `json:"reason"`
	EmergencyOverrideBy     *string         `json:"emergency_override_by"`
	EmergencyOverrideReason string          `json:"emergency_override_reason"`
	EmergencyOverrideAt     *time.Time      `json:"emergency_override_at"`
	CanOverride             bool            `json:"can_override"`
	ID                      string          `json:"id"`
	WorkloadID              string          `json:"workload_id"`
	Action                  string          `json:"action"`
	State                   string          `json:"state"`
	Result                  json.RawMessage `json:"result"`
	CreatedAt               time.Time       `json:"created_at"`
	CancelRequested         bool            `json:"cancel_requested"`
	ApprovalRequired        bool            `json:"approval_required"`
	ApprovedAt              *time.Time      `json:"approved_at"`
	CanApprove              bool            `json:"can_approve"`
}
