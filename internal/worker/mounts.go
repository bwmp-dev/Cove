package worker

type MountMetric struct {
	Error      string `json:"error,omitempty"`
	Path       string `json:"path"`
	Filesystem string `json:"filesystem"`
	Total      uint64 `json:"total"`
	Available  uint64 `json:"available"`
	Known      bool   `json:"known"`
}
