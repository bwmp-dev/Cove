package control

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPanelRecoveryPlanSealsAndValidatesLocally(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	plan := panelHostPlan{Kind: "cove.panel-backup.v1", JobID: "job", AgentID: "agent", Action: "backup", Config: panelBackupConfig{IntervalHours: 24, KeepLast: 7, KeepDays: 30, MaxBytes: 1 << 30}}
	plain, _ := json.Marshal(plan)
	var sealed, opened bytes.Buffer
	if err := PanelRecoveryPlan(bytes.NewReader(plain), &sealed, key, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed.String(), "agent") {
		t.Fatal("sealed plan exposed its contents")
	}
	if err := PanelRecoveryPlan(bytes.NewReader(sealed.Bytes()), &opened, key, false); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Plan panelHostPlan `json:"plan"`
	}
	if err := json.Unmarshal(opened.Bytes(), &result); err != nil || result.Plan.AgentID != "agent" {
		t.Fatal("plan changed", err)
	}
	if err := PanelRecoveryPlan(bytes.NewReader(sealed.Bytes()), &bytes.Buffer{}, bytes.Repeat([]byte{8}, 32), false); err == nil {
		t.Fatal("wrong key accepted")
	}
	plan.Action, plan.SnapshotID = "restore", "../snapshot"
	plain, _ = json.Marshal(plan)
	if err := PanelRecoveryPlan(bytes.NewReader(plain), &bytes.Buffer{}, key, true); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	if err := PanelRecoveryPlan(strings.NewReader(strings.Repeat("x", panelPlanLimit+1)), &bytes.Buffer{}, key, false); err == nil {
		t.Fatal("unbounded plan accepted")
	}
}

func TestPanelHostRequiresSeparateSignedAuthentication(t *testing.T) {
	s := &Server{Key: bytes.Repeat([]byte{7}, 32), AdminToken: strings.Repeat("t", 32)}
	body := []byte(`{"agent_id":"agent"}`)
	for _, scenario := range []string{"valid", "cookie", "changed-body", "wrong-path", "expired", "query", "wrong-token"} {
		t.Run(scenario, func(t *testing.T) {
			stamp := strconv.FormatInt(time.Now().Unix(), 10)
			if scenario == "expired" {
				stamp = strconv.FormatInt(time.Now().Add(-6*time.Minute).Unix(), 10)
			}
			req := httptest.NewRequest("POST", "/api/internal/panel-host/claim", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+s.AdminToken)
			req.Header.Set("X-Cove-Host-Time", stamp)
			req.Header.Set("X-Cove-Host-Signature", panelHostSignature(s.Key, stamp, req.Method, req.URL.Path, body))
			switch scenario {
			case "cookie":
				req.Header.Del("Authorization")
				req.Header.Set("Cookie", "cove_session=synthetic")
			case "changed-body":
				req.Body = httptest.NewRequest("POST", "/", strings.NewReader(`{}`)).Body
			case "wrong-path":
				req.URL.Path += "/report"
			case "query":
				req.URL.RawQuery = "other=1"
			case "wrong-token":
				req.Header.Set("Authorization", "Bearer wrong")
			}
			_, ok := s.authenticatePanelHost(httptest.NewRecorder(), req)
			if ok != (scenario == "valid") {
				t.Fatalf("accepted=%v", ok)
			}
		})
	}
}
