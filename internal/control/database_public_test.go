package control

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDatabaseTLSBindingsDeliverOnlyPublicTrustAndKeepKeysSealed(t *testing.T) {
	f, id := managedDatabaseFixture(t, "")
	app := jsonString(t, f.call("root", "default", "POST", "/api/workloads", workloadInput{Name: "TLS consumer", NodeID: "managed-node", ProjectID: "managed-project", Compose: "services:\n  app:\n    image: busybox:1.37\n", Environment: map[string]string{"DATABASE_URL": "{{resource.DATABASE}}"}}, 201), "id")
	f.call("root", "default", "PUT", "/api/workloads/"+app+"/variable-bindings", resourceVariableBinding{Name: "DATABASE", SourceID: id, SourceKey: "DB_URL"}, 200)
	certificate := databaseTestCertificate(t, "data.example.com", time.Now().Add(48*time.Hour))
	metadata := f.call("root", "default", "GET", "/api/workloads/"+id+"/database", nil, 200)
	body := map[string]any{"enabled": true, "confirm": true, "revision": jsonString(t, metadata, "revision"), "hostname": certificate.Hostname, "bind_address": certificate.BindAddress, "port": certificate.Port, "certificate": certificate.Certificate, "private_key": certificate.PrivateKey}
	f.call("root", "default", "POST", "/api/workloads/"+id+"/database/public-access", body, 202)
	config := readDatabaseConfig(t, f, id)
	certificates, err := f.s.databaseBindingCAs(context.Background(), app)
	if err != nil || len(certificates) != 1 || certificates[databaseCAFile(config.Database)] != certificate.Certificate {
		t.Fatal("binding did not deliver the selected CA", err)
	}
	values, err := f.s.resolveSharedVariables(context.Background(), app, map[string]string{"URL": "{{resource.DATABASE}}"})
	if err != nil || !strings.Contains(values["URL"], "sslmode=verify-full") || !strings.Contains(values["URL"], databaseCAFile(config.Database)) {
		t.Fatal("bound URL does not require the delivered CA", err)
	}
	metadata = f.call("root", "default", "GET", "/api/workloads/"+id+"/database", nil, 200)
	if strings.Contains(metadata.Body.String(), "PRIVATE KEY") || !strings.Contains(metadata.Body.String(), "tls_ca") {
		t.Fatal("TLS metadata leaked the key or omitted public trust")
	}
	credentials := f.call("root", "default", "GET", "/api/workloads/"+id+"/database/credentials", nil, 200)
	if strings.Contains(credentials.Body.String(), "PRIVATE KEY") {
		t.Fatal("credential reveal returned a TLS private key")
	}
}

func databaseTestCertificate(t *testing.T, hostname string, expires time.Time) databasePublicInput {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: hostname}, DNSNames: []string{hostname}, NotBefore: time.Now().Add(-time.Hour), NotAfter: expires, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return databasePublicInput{Hostname: hostname, BindAddress: "127.0.0.1", Port: 16432, Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))}
}

func TestDatabasePublicAccessRequiresMatchingCurrentCertificateAndHasPrivateRecovery(t *testing.T) {
	for _, engine := range databaseEngines {
		t.Run(engine.id, func(t *testing.T) {
			config := databaseInput(t, engine.id, "node", "project")
			if err := prepareDatabase(&config, "11111111111111111111111111111111"); err != nil {
				t.Fatal(err)
			}
			privateHost := config.Database.Host
			valid := databaseTestCertificate(t, "data.example.com", time.Now().Add(48*time.Hour))
			bad := valid
			bad.Hostname = "other.example.com"
			if configureDatabasePublic(&config, true, bad) == nil {
				t.Fatal("wrong hostname accepted")
			}
			bad = valid
			bad.PrivateKey = "not a private key"
			if configureDatabasePublic(&config, true, bad) == nil {
				t.Fatal("invalid key accepted")
			}
			if err := configureDatabasePublic(&config, true, valid); err != nil {
				t.Fatal(err)
			}
			if err := validateWorkload(config); err != nil {
				t.Fatal(err)
			}
			if config.Database.PublicAccess == nil || config.Database.Host != valid.Hostname || config.Database.PrivateHost != privateHost || strings.Contains(config.Compose, "PRIVATE KEY") || len(config.FileSecrets) < 3 {
				t.Fatal("TLS secrets or hostname configuration incorrect")
			}
			values, err := databaseExports(config, nil)
			if err != nil {
				t.Fatal(err)
			}
			publicURL, err := url.Parse(values["DB_PUBLIC_URL"])
			if err != nil || publicURL.Hostname() != valid.Hostname || publicURL.Port() != "16432" || values["DB_TLS_CA"] != valid.Certificate {
				t.Fatal("public connection exports differ from selected TLS listener", err)
			}
			if err := configureDatabasePublic(&config, false, databasePublicInput{}); err != nil {
				t.Fatal(err)
			}
			if err := validateWorkload(config); err != nil {
				t.Fatal(err)
			}
			if config.Database.PublicAccess != nil || config.Database.Host != privateHost || len(config.FileSecrets) != 0 || strings.Contains(config.Compose, "published:") {
				t.Fatal("disabling exposure did not recover the private configuration")
			}
			for key := range config.Environment {
				if strings.HasPrefix(key, "COVE_DB_TLS_") {
					t.Fatal("disabled TLS retained private key material")
				}
			}
		})
	}
	expired := databaseTestCertificate(t, "data.example.com", time.Now().Add(-time.Minute))
	if _, _, err := validateDatabaseCertificate(expired); err == nil {
		t.Fatal("expired certificate accepted")
	}
	valid := databaseTestCertificate(t, "data.example.com", time.Now().Add(48*time.Hour))
	foreign := databaseTestCertificate(t, "data.example.com", time.Now().Add(48*time.Hour))
	valid.CACertificate = foreign.Certificate
	if _, _, err := validateDatabaseCertificate(valid); err == nil {
		t.Fatal("untrusted certificate chain accepted")
	}
	valid.CACertificate = valid.Certificate
	valid.Certificate += "\n" + valid.PrivateKey
	if _, _, err := validateDatabaseCertificate(valid); err == nil {
		t.Fatal("server certificate field accepted private key material")
	}
}

func TestDatabaseCopiesStartPrivateWithoutMutatingSource(t *testing.T) {
	for _, engine := range databaseEngines {
		t.Run(engine.id, func(t *testing.T) {
			original := databaseInput(t, engine.id, "node", "project")
			if err := prepareDatabase(&original, "11111111111111111111111111111111"); err != nil {
				t.Fatal(err)
			}
			certificate := databaseTestCertificate(t, "data.example.com", time.Now().Add(48*time.Hour))
			if err := configureDatabasePublic(&original, true, certificate); err != nil {
				t.Fatal(err)
			}
			copy := original
			if err := isolateDatabaseCopy(&copy, "22222222222222222222222222222222"); err != nil {
				t.Fatal(err)
			}
			if copy.Database.PublicAccess != nil || strings.Contains(copy.Compose, "published:") || copy.Environment["COVE_DB_TLS_KEY"] != "" || copy.Database.Host == original.Database.Host {
				t.Fatal("copy inherited exposure or certificates")
			}
			if original.Database.PublicAccess == nil || original.Environment["COVE_DB_TLS_KEY"] != certificate.PrivateKey {
				t.Fatal("copy isolation mutated source")
			}
			if err := validateWorkload(copy); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDatabasePrivateRecoveryRetainsItsPinnedImageAcrossCatalogueUpdates(t *testing.T) {
	config := databaseInput(t, "postgresql", "node", "project")
	if err := prepareDatabase(&config, "11111111111111111111111111111111"); err != nil {
		t.Fatal(err)
	}
	previous := config.Database.Image
	config.Database.Image, config.Database.Version = "postgres:18.6-alpine", "18.6-alpine"
	config.Compose = strings.ReplaceAll(config.Compose, previous, config.Database.Image)
	certificate := databaseTestCertificate(t, "data.example.com", time.Now().Add(48*time.Hour))
	if err := configureDatabasePublic(&config, true, certificate); err != nil {
		t.Fatal(err)
	}
	if err := configureDatabasePublic(&config, false, databasePublicInput{}); err != nil {
		t.Fatal(err)
	}
	if config.Database.PublicAccess != nil || config.Database.Image != "postgres:18.6-alpine" || !strings.Contains(config.Compose, "postgres:18.6-alpine") {
		t.Fatal("private recovery changed the retained image")
	}
	if err := validateWorkload(config); err != nil {
		t.Fatal(err)
	}
}
