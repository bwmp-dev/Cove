package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
)

var errRouteProtocolUnsupported = errors.New("upgrade and verify the worker before using these route features")

func checkRouteProtocol(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, workload string, routes []worker.Route) error {
	if worker.RouteProtocol(routes) < 2 {
		return nil
	}
	var version int
	if err := db.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'route_protocol')::integer,0) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, workload).Scan(&version); err != nil {
		return err
	}
	if version < worker.RouteProtocol(routes) {
		return errRouteProtocolUnsupported
	}
	return nil
}

func (r *routeInput) UnmarshalJSON(data []byte) error {
	type input routeInput
	var decoded input
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var fields struct {
		Policy map[string]json.RawMessage `json:"policy"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*r = routeInput(decoded)
	r.extensionFields = fields.Policy
	return nil
}

func retainRouteExtensions(target *worker.Route, input routeInput, old worker.Route, ambiguous bool) error {
	if target.Protocol == "" && old.Protocol == "http" {
		target.Protocol = old.Protocol
	}
	previous := worker.RoutePolicy{}
	if old.Policy != nil {
		previous = *old.Policy
	}
	policy := worker.RoutePolicy{}
	if target.Policy != nil {
		policy = *target.Policy
	}
	provided := func(name string, nonempty bool) bool {
		_, present := input.extensionFields[name]
		return present || nonempty
	}
	var incoming worker.RoutePolicy
	if input.Policy != nil {
		incoming = *input.Policy
	}
	for _, headers := range []struct {
		next  *map[string]string
		old   map[string]string
		input map[string]string
	}{
		{&policy.RequestHeaders, previous.RequestHeaders, incoming.RequestHeaders},
		{&policy.ResponseHeaders, previous.ResponseHeaders, incoming.ResponseHeaders},
	} {
		values := make(map[string]string, len(*headers.next))
		for name, value := range *headers.next {
			if headers.input[name] == worker.RedactedRouteHeader {
				var found bool
				for key, saved := range headers.old {
					if strings.EqualFold(name, key) {
						value, found = saved, true
						break
					}
				}
				if !found {
					return errors.New("replace the hidden header value before adding it to a different route")
				}
				if ambiguous && (*headers.next)[name] != worker.RedactedRouteHeader && (*headers.next)[name] != value {
					return errors.New("set headers explicitly when changing a path with different header values")
				}
			}
			values[name] = value
		}
		if *headers.next != nil {
			*headers.next = values
		}
	}
	if !provided("certificate_source", incoming.CertificateSource != "") && !provided("certificate_id", incoming.CertificateID != "") {
		if ambiguous && (policy.CertificateID != previous.CertificateID || (policy.CertificateSource == "default") != (previous.CertificateSource == "default")) {
			return errors.New("set the certificate explicitly when changing a path with different certificate settings")
		}
		if previous.CertificateID != "" || previous.CertificateSource == "default" {
			policy.CertificateID, policy.CertificateSource = previous.CertificateID, previous.CertificateSource
		}
	}
	if !provided("timeouts", incoming.Timeouts != nil) {
		if ambiguous && !reflect.DeepEqual(policy.Timeouts, previous.Timeouts) {
			return errors.New("set upstream timeouts explicitly when changing paths with different settings")
		}
		policy.Timeouts = previous.Timeouts
	}
	if !provided("visibility", incoming.Visibility != "") && !provided("listener", incoming.Listener != "") {
		if ambiguous && (policy.Visibility != previous.Visibility || policy.Listener != previous.Listener) {
			return errors.New("set route visibility and listener explicitly when changing paths with different privacy settings")
		}
		policy.Visibility, policy.Listener = previous.Visibility, previous.Listener
	}
	if !provided("forward_auth", incoming.ForwardAuth != nil) {
		if ambiguous && !reflect.DeepEqual(policy.ForwardAuth, previous.ForwardAuth) {
			return errors.New("set forward-auth explicitly when changing a path with different route protections")
		}
		policy.ForwardAuth = previous.ForwardAuth
	} else if policy.ForwardAuth != nil && previous.ForwardAuth != nil {
		auth := *policy.ForwardAuth
		var fields map[string]json.RawMessage
		if raw := input.extensionFields["forward_auth"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &fields); err != nil {
				return err
			}
		}
		if auth.URL == "" && auth.Service == "" {
			auth.URL = previous.ForwardAuth.URL
			auth.Service, auth.Port = previous.ForwardAuth.Service, previous.ForwardAuth.Port
		}
		if _, supplied := fields["path"]; !supplied && auth.Path == "" && auth.Service == previous.ForwardAuth.Service {
			auth.Path = previous.ForwardAuth.Path
		}
		policy.ForwardAuth = &auth
	}
	if !provided("maintenance", incoming.Maintenance != nil) {
		if ambiguous && !reflect.DeepEqual(policy.Maintenance, previous.Maintenance) {
			return errors.New("set maintenance explicitly when changing a path with different route protections")
		}
		policy.Maintenance = previous.Maintenance
	}
	if !provided("error_pages", incoming.ErrorPages != nil) {
		if ambiguous && !reflect.DeepEqual(policy.ErrorPages, previous.ErrorPages) {
			return errors.New("set error pages explicitly when changing a path with different route settings")
		}
		policy.ErrorPages = previous.ErrorPages
	}
	encode := func(value worker.RoutePolicy) (map[string]json.RawMessage, error) {
		data, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		var fields map[string]json.RawMessage
		err = json.Unmarshal(data, &fields)
		return fields, err
	}
	oldFields, err := encode(previous)
	if err != nil {
		return err
	}
	newFields, err := encode(policy)
	if err != nil {
		return err
	}
	incomingFields, err := encode(incoming)
	if err != nil {
		return err
	}
	for name, value := range oldFields {
		if name == "forward_auth" || name == "maintenance" || name == "error_pages" || name == "certificate_source" || name == "certificate_id" {
			continue
		}
		if _, nonempty := incomingFields[name]; provided(name, nonempty) {
			continue
		}
		if ambiguous && !bytes.Equal(newFields[name], value) {
			return errors.New("set " + strings.ReplaceAll(name, "_", "-") + " explicitly when changing a path with different route policies")
		}
		newFields[name] = value
	}
	data, err := json.Marshal(newFields)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &policy); err != nil {
		return err
	}
	if target.Policy != nil || len(newFields) > 0 {
		target.Policy = &policy
	}
	return nil
}
