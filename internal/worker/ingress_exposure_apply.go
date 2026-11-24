package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

func (e *Engine) recoverIngressExposure(ctx context.Context, client *http.Client, state *ingressExposureState) error {
	change := state.Change
	if change == nil || change.FinishedAt != nil {
		return nil
	}
	if err := cleanupIngressExposureValidator(ctx, client, change); err != nil {
		return err
	}
	if err := e.cleanupIngressExposureStaging(change); err != nil {
		return err
	}
	change.Validator, change.Staging = "", ""
	if change.Committed {
		if err := finishIngressProxy(ctx, client, change); err != nil {
			return err
		}
		change.Phase = "completed"
	} else {
		if err := restoreIngressProxy(ctx, client, change); err != nil {
			return err
		}
		change.Phase = "interrupted"
	}
	now := time.Now().UTC()
	change.FinishedAt = &now
	return e.saveIngressExposures(*state)
}

func (e *Engine) applyIngressExposure(ctx context.Context, client *http.Client, prepared preparedIngressExposure, state *ingressExposureState) (resultErr error) {
	if state.Change != nil && state.Change.FinishedAt == nil {
		return errors.New("the previous proxy change must settle first")
	}
	if !prepared.Plan.RequiresRestart {
		return nil
	}
	change := &ingressProxyChange{Digest: prepared.Plan.Digest, ID: prepared.Plan.ChangeID, Phase: "validating", Previous: &prepared.Snapshot, Desired: prepared.Plan.Settings, DesiredPorts: prepared.Plan.Ports, StartedAt: time.Now().UTC()}
	state.Change = change
	if err := e.saveIngressExposures(*state); err != nil {
		return err
	}
	defer func() {
		if resultErr == nil {
			return
		}
		durable, err := e.loadIngressExposures()
		if err != nil || durable.Change == nil || durable.Change.ID != change.ID {
			resultErr = errors.Join(resultErr, errors.New("proxy recovery checkpoint could not be confirmed"))
			return
		}
		*state = durable
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := e.recoverIngressExposure(recovery, client, state); err != nil {
			resultErr = errors.Join(resultErr, errors.New("proxy recovery is still pending; conflicting ingress changes remain blocked"))
		}
	}()
	if err := e.validateIngressExposure(ctx, client, prepared, state); err != nil {
		return err
	}
	if err := e.validateStoppedIngressBindings(ctx, client, prepared, state); err != nil {
		return err
	}
	if prepared.Snapshot.Launch.Kind == "file" {
		if err := stageIngressExposureStatic(ctx, client, prepared); err != nil {
			return err
		}
	}
	var expected map[string]rawObject
	if prepared.Snapshot.Running {
		var err error
		expected, err = e.liveRawData(ctx)
		if err != nil {
			return errors.New("current proxy runtime could not be confirmed before replacement")
		}
	}
	change.Phase = "applying"
	if err := e.saveIngressExposures(*state); err != nil {
		return err
	}
	original := prepared.Snapshot.ID
	if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+original+"/update", map[string]any{"RestartPolicy": map[string]string{"Name": "no"}}, nil); err != nil {
		return err
	}
	if prepared.Snapshot.Running {
		if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+original+"/stop?t=10", nil, nil); err != nil {
			return err
		}
	}
	if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+original+"/rename?name=cove-ingress-backup-"+change.ID, nil, nil); err != nil {
		return err
	}
	for _, network := range prepared.Snapshot.Networks {
		if err := ingressDockerJSON(ctx, client, "POST", "/networks/"+network.NetworkID+"/disconnect", map[string]any{"Container": original, "Force": true}, nil); err != nil {
			return err
		}
	}
	var created struct{ ID string }
	if err := ingressDockerJSON(ctx, client, "POST", "/containers/create?name=cove-ingress", prepared.Body, &created); err != nil {
		return err
	}
	if len(created.ID) != 64 || !containerIdentity.MatchString(created.ID) {
		return errors.New("replacement proxy identity was not returned")
	}
	change.Candidate = created.ID
	if err := e.saveIngressExposures(*state); err != nil {
		return err
	}
	if prepared.Snapshot.Running {
		if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+created.ID+"/start", nil, nil); err != nil {
			return err
		}
		if err := e.confirmIngressExposure(ctx, prepared, expected); err != nil {
			return err
		}
	}
	installed, err := captureIngressProxy(ctx, client)
	if err != nil || installed.ID != change.Candidate {
		return errors.New("installed listener proxy configuration could not be captured for recovery")
	}
	change.Installed = &installed
	state.Settings, state.Ports = change.Desired, change.DesiredPorts
	change.Committed, change.Phase = true, "cleanup"
	if err := e.saveIngressExposures(*state); err != nil {
		return err
	}
	return e.recoverIngressExposure(ctx, client, state)
}

func (e *Engine) confirmIngressExposure(ctx context.Context, prepared preparedIngressExposure, expected map[string]rawObject) error {
	client := e.ingressDockerClient()
	defer client.CloseIdleConnections()
	snapshot, err := captureIngressProxy(ctx, client)
	if err != nil || snapshot.Image != prepared.Snapshot.Image || !snapshot.Running {
		return errors.New("replacement proxy image and running state were not confirmed")
	}
	if len(snapshot.Networks) != len(prepared.Snapshot.Networks) {
		return errors.New("replacement proxy network attachments were not preserved")
	}
	for name, old := range prepared.Snapshot.Networks {
		next := snapshot.Networks[name]
		if next.NetworkID != old.NetworkID || old.IPAddress != "" && next.IPAddress != old.IPAddress || old.GlobalIPv6Address != "" && next.GlobalIPv6Address != old.GlobalIPv6Address || old.MacAddress != "" && next.MacAddress != old.MacAddress {
			return errors.New("replacement proxy peer addresses were not preserved")
		}
	}
	deadline := time.Now().Add(customConfirmTimeout)
	reason := "proxy runtime API is unavailable"
	for {
		live, err := e.liveRawData(ctx)
		if err == nil {
			reason = ingressExposureDifference(live, expected)
		}
		if err == nil && liveMatches(live, expected, nil) {
			valid := true
			for _, object := range live {
				valid = valid && object.Status != "disabled" && len(object.Errors) == 0
			}
			if valid {
				reason = "entrypoint inventory is unavailable"
				data, err := e.proxyAPI(ctx, "cove-ingress", "/api/entrypoints")
				var entries []struct{ Name, Address string }
				if err == nil && json.Unmarshal(data, &entries) == nil {
					reason = "planned listener names or addresses were not loaded"
					found := map[string]bool{}
					for _, entry := range entries {
						found[entry.Name+"/"+entry.Address] = true
					}
					for _, listener := range prepared.Effective.Listeners {
						port := prepared.Plan.Ports[listener.ID]
						protocol := "tcp"
						if listener.Protocol == "udp" {
							protocol = "udp"
						}
						valid = valid && found[ingressListenerName(listener.ID)+"/:"+strconv.Itoa(port)+"/"+protocol]
					}
					if valid {
						return e.probeIngressExposureTLS(ctx, client, snapshot.ID, prepared.Plan.ChangeID, prepared.Probes)
					}
				}
			} else {
				reason = "runtime objects are disabled or invalid"
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("replacement proxy did not confirm the previous routes and planned listeners: %s", reason)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
