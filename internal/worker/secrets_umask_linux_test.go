package worker

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestFileSecretPrivateUmask(t *testing.T) {
	if os.Getenv("COVE_TEST_PRIVATE_UMASK") == "1" {
		syscall.Umask(0077)
		TestFileSecretsSelectedServicesAndNoInlineValues(t)
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestFileSecretPrivateUmask$")
	command.Env = append(os.Environ(), "COVE_TEST_PRIVATE_UMASK=1")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("private umask fixture: %v\n%s", err, out)
	}
}
