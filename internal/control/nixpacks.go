package control

import (
	"context"
	"cove/internal/worker"
	"encoding/json"
	"errors"
)

func readNixpacksSupport(ctx context.Context, query buildNodeQuery, node string) (worker.NixpacksSupport, error) {
	var state string
	var data []byte
	if err := query.QueryRow(ctx, `SELECT state,COALESCE(capabilities,'{}') FROM nodes WHERE id=$1`, node).Scan(&state, &data); err != nil {
		return worker.NixpacksSupport{}, err
	}
	var probe worker.Probe
	if json.Unmarshal(data, &probe) != nil {
		return worker.NixpacksSupport{}, errors.New("Nixpacks capabilities are unavailable")
	}
	return worker.NixpacksDetails(probe, state == "ready"), nil
}

func requireNixpacks(ctx context.Context, query buildNodeQuery, node string) error {
	status, err := readNixpacksSupport(ctx, query, node)
	if err != nil {
		return errors.New("Nixpacks capabilities are unavailable")
	}
	if !status.Supported {
		return errors.New(status.Reason)
	}
	return nil
}
