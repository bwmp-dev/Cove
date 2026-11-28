package control

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"cove/internal/outbound"
	"cove/internal/worker"
)

const panelPlanLimit = 1 << 20

type panelBackupConfig struct {
	Enabled       bool   `json:"enabled"`
	IntervalHours int    `json:"interval_hours"`
	KeepLast      int    `json:"keep_last"`
	KeepDays      int    `json:"keep_days"`
	MaxBytes      int64  `json:"max_bytes"`
	StorageID     string `json:"storage_id"`
	Password      string `json:"password,omitempty"`
}

type panelHostPlan struct {
	OutboundProxy *outbound.Config        `json:"outbound_proxy,omitempty"`
	Kind          string                  `json:"kind"`
	JobID         string                  `json:"job_id"`
	AgentID       string                  `json:"agent_id"`
	Action        string                  `json:"action"`
	SnapshotID    string                  `json:"snapshot_id,omitempty"`
	ActorID       string                  `json:"actor_id"`
	Config        panelBackupConfig       `json:"config"`
	Storage       *worker.StorageLocation `json:"storage,omitempty"`
}

func validatePanelBackupConfig(config panelBackupConfig) error {
	if config.IntervalHours < 1 || config.IntervalHours > 8760 || config.KeepLast < 1 || config.KeepLast > 100 || config.KeepDays < 1 || config.KeepDays > 365 || config.MaxBytes < 64<<20 || config.MaxBytes > worker.MaxTransferBytes {
		return errors.New("choose a 1–8760 hour interval, 1–100 snapshots, 1–365 days and a 64 MiB–64 GiB snapshot limit")
	}
	if config.StorageID != "" && (len(config.Password) < 16 || len(config.Password) > 4096 || strings.ContainsRune(config.Password, 0)) {
		return errors.New("offsite copy requires an independent encryption password of 16–4096 bytes")
	}
	if len(config.StorageID) > 64 || strings.IndexFunc(config.StorageID, unicode.IsControl) >= 0 {
		return errors.New("invalid panel backup storage location")
	}
	return nil
}

func panelPlanID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func validPanelSnapshot(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && len(value) == 64 && strings.ToLower(value) == value
}

func validatePanelHostPlan(plan panelHostPlan) error {
	if plan.OutboundProxy != nil && plan.OutboundProxy.Validate() != nil {
		return errors.New("invalid panel backup proxy settings")
	}
	if plan.Kind != "cove.panel-backup.v1" || !panelPlanID(plan.JobID) || !panelPlanID(plan.AgentID) || len(plan.ActorID) > 256 || !utf8.ValidString(plan.ActorID) || strings.IndexFunc(plan.ActorID, unicode.IsControl) >= 0 {
		return errors.New("invalid panel recovery plan identity")
	}
	if plan.Action != "backup" && plan.Action != "restore" && plan.Action != "resume_updates" {
		return errors.New("invalid panel recovery action")
	}
	if plan.Action == "restore" && !validPanelSnapshot(plan.SnapshotID) || plan.Action != "restore" && plan.SnapshotID != "" {
		return errors.New("restore requires an exact retained snapshot identity")
	}
	if err := validatePanelBackupConfig(plan.Config); err != nil {
		return err
	}
	if plan.Config.StorageID == "" && plan.Storage != nil || plan.Config.StorageID != "" && plan.Storage == nil {
		return errors.New("panel backup storage does not match its configuration")
	}
	if plan.Storage != nil {
		return worker.ValidateStorage(plan.Storage)
	}
	return nil
}

func PanelRecoveryPlan(input io.Reader, output io.Writer, key []byte, encode bool) error {
	if len(key) != 32 {
		return errors.New("panel recovery plan key unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(input, panelPlanLimit+1))
	if err != nil || len(data) > panelPlanLimit {
		return errors.New("panel recovery plan exceeds its limit")
	}
	var plan panelHostPlan
	if encode {
		if json.Unmarshal(data, &plan) != nil || validatePanelHostPlan(plan) != nil {
			return errors.New("invalid panel recovery plan")
		}
		ciphertext, err := seal(key, data)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(map[string]string{"sealed": base64.StdEncoding.EncodeToString(ciphertext)})
	}
	var envelope struct {
		Sealed string `json:"sealed"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return errors.New("invalid sealed panel recovery plan")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(envelope.Sealed)
	if err != nil {
		return errors.New("invalid sealed panel recovery plan")
	}
	plain, err := unseal(key, ciphertext)
	if err != nil || json.Unmarshal(plain, &plan) != nil || validatePanelHostPlan(plan) != nil {
		return errors.New("panel recovery plan could not be authenticated")
	}
	transport, err := worker.PanelBackupTransport(plan.Storage)
	if err != nil {
		return err
	}
	if plan.Storage != nil && plan.OutboundProxy != nil {
		values, err := worker.StorageProxyEnvironment(plan.Storage, *plan.OutboundProxy)
		if err != nil {
			return err
		}
		transport.Environment = append(transport.Environment, values...)
	}
	return json.NewEncoder(output).Encode(map[string]any{"plan": plan, "transport": transport})
}

func panelHostSignature(key []byte, timestamp, method, path string, body []byte) string {
	derive := hmac.New(sha256.New, key)
	derive.Write([]byte("cove.panel.host.v1"))
	mac := hmac.New(sha256.New, derive.Sum(nil))
	mac.Write([]byte(timestamp + "\n" + method + "\n" + path + "\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) authenticatePanelHost(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	actual, expected := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(s.AdminToken))
	timestamp := r.Header.Get("X-Cove-Host-Time")
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if !bearer || len(s.AdminToken) < 32 || len(s.Key) != 32 || !hmac.Equal(actual[:], expected[:]) || err != nil || r.Method != http.MethodPost || r.URL.RawQuery != "" || seconds < time.Now().Unix()-300 || seconds > time.Now().Unix()+300 {
		http.Error(w, "authenticated panel host agent required", 403)
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, panelPlanLimit+1))
	if err != nil || len(data) > panelPlanLimit {
		http.Error(w, "host report exceeds its limit", 400)
		return nil, false
	}
	signature, err := hex.DecodeString(r.Header.Get("X-Cove-Host-Signature"))
	want, _ := hex.DecodeString(panelHostSignature(s.Key, timestamp, r.Method, r.URL.Path, data))
	if err != nil || !hmac.Equal(signature, want) {
		http.Error(w, "authenticated panel host agent required", 403)
		return nil, false
	}
	return data, true
}
