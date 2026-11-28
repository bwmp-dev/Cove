package worker

import (
	"slices"
	"testing"

	"cove/internal/outbound"
)

func TestOperationsMergeStorageProxyEnvironment(t *testing.T) {
	c := outbound.Config{HTTP: "http://proxy.invalid:3128", HTTPS: "http://secure-proxy.invalid:3128"}
	for _, protocol := range []string{"s3", "b2", "webdav", "sftp"} {
		s := &StorageLocation{Protocol: protocol, Endpoint: "sftp://storage.invalid:2222"}
		environment, err := StorageProxyEnvironment(s, c)
		if err != nil || !slices.Contains(environment, "HTTPS_PROXY="+c.HTTPS) || !slices.Contains(environment, "HTTP_PROXY="+c.HTTP) {
			t.Fatal(protocol, environment, err)
		}
		if protocol == "sftp" && !slices.Contains(environment, "RCLONE_CONFIG_COVE_HTTP_PROXY="+c.HTTPS) {
			t.Fatal("SFTP must select its explicit CONNECT proxy", environment)
		}
	}
	c.NoProxy = "storage.invalid"
	environment, err := StorageProxyEnvironment(&StorageLocation{Protocol: "sftp", Endpoint: "sftp://storage.invalid:2222"}, c)
	if err != nil || !slices.Contains(environment, "RCLONE_CONFIG_COVE_HTTP_PROXY=") {
		t.Fatal("SFTP explicit bypass was not respected", environment, err)
	}
}
