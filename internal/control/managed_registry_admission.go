package control

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
)

func managedRegistryBusy(err error) bool {
	var constraint *pgconn.PgError
	return errors.As(err, &constraint) && constraint.Code == "23514" && constraint.ConstraintName == "cove_managed_registry_busy"
}

func managedRegistryAdmissionError(w http.ResponseWriter, err error) bool {
	if !managedRegistryBusy(err) {
		return false
	}
	http.Error(w, "managed registry maintenance or verification is active; wait for it to finish before publishing or selecting the registry", 409)
	return true
}
