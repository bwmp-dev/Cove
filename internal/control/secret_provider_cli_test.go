package control

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestSecretProviderCLIHasIsolatedCredentialsAndReadonlyArguments(t *testing.T) {
	t.Setenv("BWS_ACCESS_TOKEN", "ambient-not-allowed")
	t.Setenv("BWS_SERVER_URL", "http://ambient.invalid")
	t.Setenv("OP_CONNECT_TOKEN", "ambient-not-allowed")
	t.Setenv("OP_CONNECT_HOST", "http://ambient.invalid")
	for _, kind := range []string{"onepassword-service", "bitwarden"} {
		t.Run(kind, func(t *testing.T) {
			ref := "vault/item/password"
			if kind == "bitwarden" {
				ref = "2863ced6-eba1-48b4-b5c0-afa30104877a"
			}
			var configPath string
			token := "new-provider-credential"
			if kind == "onepassword-service" {
				token = "ops_" + base64.RawURLEncoding.EncodeToString([]byte(`{"signInAddress":"fixture.1password.com"}`))
			}
			source := cliVariableSource{config: secretProviderConfig{Kind: kind, Token: token, URL: "https://vault.bitwarden.eu"}, command: func(ctx context.Context, name string, args, environment []string) ([]byte, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("CLI has no deadline")
				}
				if strings.Contains(strings.Join(args, " "), token) {
					t.Fatal("credential in argv")
				}
				joined := strings.Join(environment, "\n")
				if strings.Contains(joined, "ambient-not-allowed") || strings.Contains(joined, "ambient.invalid") {
					t.Fatal("ambient provider settings inherited")
				}
				if kind == "onepassword-service" {
					if name != "op" || strings.Join(args, " ") != "read --no-newline op://vault/item/password" || !strings.Contains(joined, "OP_SERVICE_ACCOUNT_TOKEN="+token) {
						t.Fatal("wrong op invocation")
					}
					for _, entry := range environment {
						if strings.HasPrefix(entry, "OP_CONFIG_DIR=") {
							configPath = strings.TrimPrefix(entry, "OP_CONFIG_DIR=")
							if _, err := os.Stat(configPath); err != nil {
								t.Fatal(err)
							}
						}
					}
					return []byte("cli-secret\nwith-newline"), nil
				}
				if name != "bws" || !strings.Contains(joined, "BWS_ACCESS_TOKEN=new-provider-credential") {
					t.Fatal("wrong bws invocation")
				}
				configPath = args[1]
				config, err := os.ReadFile(configPath)
				if err != nil || !strings.Contains(string(config), "https://api.bitwarden.eu") || !strings.Contains(string(config), "state_opt_out = true") || strings.Contains(string(config), "new-provider-credential") {
					t.Fatal("unsafe CLI configuration", err)
				}
				if strings.Join(args[2:], " ") != "--profile default --output json secret get "+ref {
					t.Fatal("CLI is not readonly")
				}
				return []byte(`{"id":"` + ref + `","value":"cli-secret\nwith-newline"}`), nil
			}}
			value, err := source.Resolve(context.Background(), ref)
			if err != nil || value != "cli-secret\nwith-newline" {
				t.Fatal("CLI secret not returned", err)
			}
			if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				t.Fatal("CLI private configuration not cleaned")
			}
			source.command = func(context.Context, string, []string, []string) ([]byte, error) {
				return nil, errors.New("raw sensitive error")
			}
			if _, err := source.Resolve(context.Background(), ref); err == nil || strings.Contains(err.Error(), "raw sensitive error") {
				t.Fatal("unsafe CLI error", err)
			}
		})
	}
}

func TestSecretProvidersRejectUnsafeEndpointsAndHiddenValues(t *testing.T) {
	for _, endpoint := range []string{"http://vault.example", "https://127.0.0.1", "https://[::1]", "https://user:pass@vault.example", "https://vault.example/?token=bad"} {
		c := secretProviderConfig{Name: "Vault", Kind: "vault", Token: "fixture-token", URL: endpoint}
		if validateSecretProvider(&c) == nil {
			t.Fatal("unsafe endpoint accepted", endpoint)
		}
	}
	c := secretProviderConfig{Name: "Bitwarden", Kind: "bitwarden", Token: "fixture-token", URL: "https://custom.example"}
	if validateSecretProvider(&c) == nil {
		t.Fatal("unrestricted vendor CLI endpoint accepted")
	}
}

func TestOnePasswordServiceTokensCannotSelectArbitraryHosts(t *testing.T) {
	for _, host := range []string{"tenant.1password.com", "tenant.1password.eu", "tenant.1password.ca"} {
		token := "ops_" + base64.RawURLEncoding.EncodeToString([]byte(`{"signInAddress":"`+host+`"}`))
		if !validOnePasswordServiceToken(token) {
			t.Fatal("documented provider address rejected")
		}
	}
	for _, host := range []string{"127.0.0.1", "localhost", "tenant.1password.com.attacker.example", "tenant.1password.com:4000", "tenant.1password.com/path", "user@tenant.1password.com", "https://tenant.1password.com", "tenant.1password.com?host=local"} {
		token := "ops_" + base64.RawURLEncoding.EncodeToString([]byte(`{"signInAddress":"`+host+`"}`))
		if validOnePasswordServiceToken(token) {
			t.Fatal("unsafe CLI destination accepted", host)
		}
	}
}
