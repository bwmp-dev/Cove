package control

import (
	"context"
	"cove/internal/worker"
	"errors"
)

func registryVerificationFailure(err error) (string, bool) {
	var remote *RemoteError
	if errors.As(err, &remote) {
		switch remote.Message {
		case worker.ErrRegistryDNSPending.Error():
			return "The registry hostname does not resolve on its node yet; a cached negative DNS answer may remain until its TTL expires. Cove will retry authenticated HTTPS verification automatically (up to 15 minutes between attempts).", true
		case worker.ErrRegistryHTTPSPending.Error():
			return "Registry deployment finished, but authenticated HTTPS verification is not ready. Cove will retry automatically (up to 15 minutes between attempts); check DNS, certificate issuance and the selected trust certificate if this persists.", true
		}
	}
	return "Registry deployment finished, but authenticated TLS verification failed. Confirm DNS, credentials and the HTTPS certificate, then retry.", false
}

func (s *Server) deferRegistryVerification(ctx context.Context, id, operation, message string) error {
	_, err := s.DB.Exec(ctx, `UPDATE managed_registries SET state='failed',last_error=$2,verification_attempts=LEAST(verification_attempts+1,6),next_verification_at=now()+make_interval(secs=>CASE verification_attempts WHEN 0 THEN 5 WHEN 1 THEN 15 WHEN 2 THEN 30 WHEN 3 THEN 60 WHEN 4 THEN 300 ELSE 900 END) WHERE id=$1 AND operation_id=$3 AND state IN ('provisioning','failed')`, id, message, operation)
	return err
}
