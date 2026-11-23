package control

import (
	"bytes"
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5"
)

var errRouteProtocolUnsupported = errors.New("upgrade and verify the worker before using maintenance pages, error pages or forward-auth")

func checkRouteProtocol(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, workload string, routes []worker.Route) error {
	if !worker.HasRouteExtensions(routes) {
		return nil
	}
	var version int
	if err := db.QueryRow(ctx, `SELECT COALESCE((n.capabilities->>'route_protocol')::integer,0) FROM nodes n JOIN workloads w ON w.node_id=n.id WHERE w.id=$1`, workload).Scan(&version); err != nil {
		return err
	}
	if version < 2 {
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
	if !provided("forward_auth", incoming.ForwardAuth != nil) {
		if ambiguous && !reflect.DeepEqual(policy.ForwardAuth, previous.ForwardAuth) {
			return errors.New("set forward-auth explicitly when changing a path with different route protections")
		}
		policy.ForwardAuth = previous.ForwardAuth
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
	if target.Policy != nil || policy.ForwardAuth != nil || policy.Maintenance != nil || policy.ErrorPages != nil {
		target.Policy = &policy
	}
	return nil
}
