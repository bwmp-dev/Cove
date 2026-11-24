package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

func (e *Engine) validateStoppedIngressBindings(ctx context.Context, client *http.Client, prepared preparedIngressExposure, state *ingressExposureState) (resultErr error) {
	if prepared.Snapshot.Running {
		return nil
	}
	if state.Change == nil || state.Change.ID != prepared.Plan.ChangeID || state.Change.Validator != "" {
		return errors.New("stopped publication validation has no exclusive checkpoint")
	}
	body, _, err := exposureValidationBody(prepared)
	if err != nil {
		return err
	}
	var host, desired map[string]json.RawMessage
	if json.Unmarshal(body["HostConfig"], &host) != nil || json.Unmarshal(prepared.Body["HostConfig"], &desired) != nil {
		return errors.New("stopped publication configuration is unreadable")
	}
	host["PortBindings"] = desired["PortBindings"]
	host["NetworkMode"], _ = json.Marshal("cove-ingress")
	host["VolumesFrom"] = json.RawMessage(`[]`)
	body["HostConfig"], _ = json.Marshal(host)
	body["NetworkingConfig"], _ = json.Marshal(map[string]any{"EndpointsConfig": map[string]any{"cove-ingress": map[string]any{}}})
	body["ExposedPorts"] = prepared.Body["ExposedPorts"]
	body["Entrypoint"], _ = json.Marshal([]string{"sh"})
	body["Cmd"], _ = json.Marshal([]string{"-ec", "sleep 60"})
	body["Env"] = json.RawMessage(`[]`)
	body["Volumes"] = json.RawMessage(`{}`)
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	state.Change.Validator = "cove-ingress-check-" + hex.EncodeToString(nonce[:])
	if err := e.saveIngressExposures(*state); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := cleanupIngressExposureValidator(cleanup, client, state.Change); err != nil {
			resultErr = errors.Join(resultErr, err)
			return
		}
		state.Change.Validator = ""
		if err := e.saveIngressExposures(*state); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	var created struct{ ID string }
	if err := ingressDockerJSON(ctx, client, "POST", "/containers/create?name="+state.Change.Validator, body, &created); err != nil {
		return err
	}
	if len(created.ID) != 64 || !containerIdentity.MatchString(created.ID) {
		return errors.New("publication probe identity was not confirmed")
	}
	if err := ingressDockerJSON(ctx, client, "POST", "/containers/"+created.ID+"/start", nil, nil); err != nil {
		return errors.New("the planned listener publications cannot bind on this node; the stopped proxy was retained")
	}
	probe, err := ingressProxyInspect(ctx, client, created.ID)
	if err != nil || probe == nil || !probe.State.Running {
		return errors.New("stopped listener publication validation did not remain running")
	}
	return nil
}
