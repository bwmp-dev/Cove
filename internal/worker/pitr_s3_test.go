package worker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func pitrS3Fixture(t *testing.T, fixture string, e *Engine) *StorageLocation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	worker := filepath.Base(fixture)
	ip, err := e.output(ctx, "docker", "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", worker)
	if err != nil || net.ParseIP(ip) == nil {
		t.Fatal("fixture address unavailable", err)
	}
	root := filepath.Join(fixture, "pitr-s3-server")
	if err = os.MkdirAll(filepath.Join(root, "data", "backups"), 0700); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Cove disposable PITR S3"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true, IPAddresses: []net.IP{net.ParseIP(ip)}}
	der, err := x509.CreateCertificate(rand.Reader, &cert, &cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"cert.pem": ca, "key.pem": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private})} {
		if err = os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	name := "cove-pitr-s3-" + worker
	err = e.Run(ctx, e.Root, []string{"docker", "run", "-d", "--name", name, "--label", "cove.test=true", "--log-driver", "none", "--network", "container:" + worker, "--volume", root + ":/srv", "rclone/rclone:1.76.0", "serve", "s3", "/srv/data", "--addr", "0.0.0.0:9911", "--cert", "/srv/cert.pem", "--key", "/srv/key.pem", "--auth-key", "fixture,synthetic-pitr-s3-password"}, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Run(context.Background(), e.Root, []string{"docker", "rm", "-f", name}, nil, io.Discard); err != nil {
			t.Error(err)
		}
	})
	endpoint := net.JoinHostPort(ip, "9911")
	for attempt := 0; attempt < 100; attempt++ {
		connection, err := net.DialTimeout("tcp", endpoint, 100*time.Millisecond)
		if err == nil {
			connection.Close()
			return &StorageLocation{Protocol: "s3", Endpoint: "https://" + endpoint, Path: "backups/pitr", AccessKey: "fixture", SecretKey: "synthetic-pitr-s3-password", Region: "us-east-1", CACertificate: string(ca)}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("disposable TLS S3 did not start")
	return nil
}
