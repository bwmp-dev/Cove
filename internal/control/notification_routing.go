package control

import (
	"errors"
	"net/http"
	"slices"
	"strings"
)

var channelOutcomeEvents = []string{
	"certificate.failed",
	"identity.provisioned", "identity.deprovisioned", "identity.membership_changed", "identity.sync_failed",
	"security.lockout", "security.new_device", "security.second_factor_reset", "audit_export.failed", "deployment.freeze_override",
	"deploy_hook.accepted", "deploy_hook.failed", "gitops.applied", "gitops.drift", "configuration.applied", "import.reviewed", "import.failed", "adoption.succeeded", "adoption.failed",
	"rollout.staged", "rollout.promoted", "rollout.aborted", "rollout.failed", "autoscale.queued", "autoscale.succeeded", "autoscale.failed", "resource.asleep", "resource.awake",
}

var notificationEvents = append([]string{
	"operation.succeeded", "operation.failed", "operation.interrupted", "operation.cancelled",
	"backup.succeeded", "backup.failed", "backup.interrupted", "backup.cancelled",
	"restore.succeeded", "restore.failed", "restore.interrupted", "restore.cancelled",
	"drill.succeeded", "drill.failed", "drill.interrupted", "drill.cancelled",
	"node_unreachable.alert", "node_unreachable.recovered", "disk_low.alert", "disk_low.recovered",
	"resource_unhealthy.alert", "resource_unhealthy.recovered", "backup_overdue.alert", "backup_overdue.recovered",
	"threshold.alert", "threshold.recovered",
	"host_key_changed.alert", "host_key_changed.recovered",
}, channelOutcomeEvents...)

func operationEvent(action, state string) string {
	if action == "pitr_backup" {
		return "backup." + state
	}
	if action == "pitr_restore" {
		return "restore." + state
	}
	if action == "backup" || action == "restore" || action == "drill" {
		return action + "." + state
	}
	return "operation." + state
}

func validateNotificationRouting(n *notificationConfig) error {
	if len(n.Events) > len(notificationEvents) {
		return errors.New("too many notification events")
	}
	if n.Owner && (n.Provider != "email" || n.Email != "") {
		return errors.New("owner routing requires an email channel without a fixed recipient")
	}
	if len(n.URL) > 4096 || len(n.Token) > 4096 || len(n.UserKey) > 256 || len(n.Target) > 256 || strings.ContainsAny(n.Token+n.UserKey+n.Target, "\r\n") {
		return errors.New("invalid notification credentials or target")
	}
	if slices.Contains([]string{"telegram", "gotify", "pushover", "matrix", "pagerduty", "opsgenie"}, n.Provider) && n.Token == "" {
		return errors.New("a provider token is required")
	}
	if slices.Contains([]string{"telegram", "matrix", "ntfy"}, n.Provider) && n.Target == "" {
		return errors.New("a channel target is required")
	}
	if n.Provider == "pushover" && n.UserKey == "" {
		return errors.New("a Pushover user key is required")
	}
	if n.Provider == "telegram" && strings.ContainsAny(n.Token, "/?#% ") {
		return errors.New("invalid Telegram bot token")
	}
	if n.Provider == "pagerduty" || n.Provider == "opsgenie" {
		if len(n.Events) == 0 || n.Success || n.Failure {
			return errors.New("incident channels require explicit alert and recovery event pairs")
		}
		for _, event := range n.Events {
			kind, suffix, ok := strings.Cut(event, ".")
			if !ok || (suffix != "alert" && suffix != "recovered") || !slices.Contains(n.Events, kind+".alert") || !slices.Contains(n.Events, kind+".recovered") {
				return errors.New("incident channels require matching alert and recovery event pairs")
			}
		}
	}
	seen := map[string]bool{}
	for _, event := range n.Events {
		if seen[event] || !slices.Contains(notificationEvents, event) {
			return errors.New("select distinct events from the notification catalogue")
		}
		seen[event] = true
	}
	if n.SigningSecret != "" && (n.Provider != "webhook" || len(n.SigningSecret) < 32 || len(n.SigningSecret) > 256) {
		return errors.New("webhook signing secrets must be 32 to 256 bytes")
	}
	if len(n.Events) == 0 && !n.Success && !n.Failure && !n.Health {
		return errors.New("select at least one notification event")
	}
	if n.Provider != "email" && n.Email != "" {
		return errors.New("email recipients require an email channel")
	}
	return nil
}

func notificationMatches(n *notificationConfig, event string) bool {
	if !n.Enabled {
		return false
	}
	if len(n.Events) > 0 {
		return slices.Contains(n.Events, event)
	}
	if slices.Contains(channelOutcomeEvents, event) {
		return false
	}
	if strings.HasSuffix(event, ".alert") || strings.HasSuffix(event, ".recovered") {
		return n.Health
	}
	if strings.HasSuffix(event, ".succeeded") {
		return n.Success
	}
	return n.Failure
}

func (s *Server) notificationCatalogue(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, notificationEvents)
}
