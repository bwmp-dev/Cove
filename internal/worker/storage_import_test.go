package worker

import (
	"bytes"
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
	"strings"
	"testing"
	"time"
)

func TestImportObjectKeyBoundaries(t *testing.T) {
	for _, key := range []string{"dump.rdb", "daily/dump..sql", "café/archive.dump"} {
		if err := ValidateImportObjectKey(key); err != nil {
			t.Fatal(key, err)
		}
	}
	for _, key := range []string{"", ".", "..", "../secret", "nested/../secret", "/other-bucket/file", "dir//file", "s3:other/file", "dir\\file", "file\x00", "file\u0085", strings.Repeat("é", 513)} {
		if ValidateImportObjectKey(key) == nil {
			t.Fatal("unsafe object key accepted", key)
		}
	}
}

func TestConfiguredS3DumpImportDocker(t *testing.T) {
	fixture := os.Getenv("COVE_DATABASE_FIXTURE")
	if fixture == "" {
		t.Skip("disposable Docker fixture required")
	}
	f := newDatabaseImportFixture(t, filepath.Join(fixture, "s3-"+filepath.Base(fixture)), "redis")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	run := func(args ...string) string {
		t.Helper()
		out, err := f.engine.output(ctx, args...)
		if err != nil {
			t.Fatal(args[:min(len(args), 3)], err, out)
		}
		return strings.TrimSpace(out)
	}
	worker := filepath.Base(fixture)
	ip := run("docker", "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", worker)
	if net.ParseIP(ip) == nil {
		t.Fatal("fixture bridge address unavailable")
	}
	server := filepath.Join(fixture, "s3-server")
	object := filepath.Join(server, "data", "backups", "team", "dumps", "redis.rdb")
	if err := os.MkdirAll(filepath.Dir(object), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, f.dump, 0600); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Cove disposable S3 fixture"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true, IPAddresses: []net.IP{net.ParseIP(ip)}}
	der, err := x509.CreateCertificate(rand.Reader, &certificate, &certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"cert.pem": ca, "key.pem": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private})} {
		if err = os.WriteFile(filepath.Join(server, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	name := "cove-s3-import-" + worker
	run("docker", "run", "-d", "--name", name, "--label", "cove.test=true", "--log-driver", "local", "--log-opt", "max-size=1m", "--log-opt", "max-file=2", "--network", "container:"+worker, "--volume", server+":/srv:ro", "rclone/rclone:1.76.0", "serve", "s3", "/srv/data", "--addr", "0.0.0.0:9910", "--cert", "/srv/cert.pem", "--key", "/srv/key.pem", "--auth-key", "fixture,synthetic-s3-import-password")
	t.Cleanup(func() {
		if err := f.engine.Run(context.Background(), f.engine.Root, []string{"docker", "rm", "-f", name}, nil, io.Discard); err != nil {
			t.Error(err)
		}
	})
	endpoint := net.JoinHostPort(ip, "9910")
	ready := false
	for attempt := 0; attempt < 90; attempt++ {
		conn, err := net.DialTimeout("tcp", endpoint, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("disposable S3 service unavailable")
	}
	storage := StorageLocation{Protocol: "s3", Endpoint: "https://" + endpoint, Path: "backups/team", AccessKey: "fixture", SecretKey: "synthetic-s3-import-password", Region: "us-east-1", CACertificate: string(ca)}
	f.set("after")
	r := f.request
	r.ID = "s3-import"
	stream := StreamRequest{Action: "backup_import_s3", Request: &r, Limit: 8 << 20, SourceStorage: &storage, ObjectKey: "dumps/redis.rdb"}
	meta, err := f.engine.receiveDatabaseImport(ctx, r, stream, bytes.NewReader(nil))
	if err != nil || meta.BackupID != r.ID || meta.Size != int64(len(f.dump)) || meta.Size < 32768 || f.value() != "after" {
		t.Fatal("configured S3 import failed or changed source data", err, meta)
	}
	for _, failure := range []string{"credentials", "tls"} {
		invalid := storage
		if failure == "credentials" {
			invalid.SecretKey = "incorrect-synthetic-password"
		} else {
			invalid.CACertificate = ""
		}
		r.ID = "bad-" + failure
		stream.SourceStorage = &invalid
		if _, err = f.engine.receiveDatabaseImport(ctx, r, stream, bytes.NewReader(nil)); err == nil {
			t.Fatal("S3 import accepted invalid", failure)
		}
	}
	r.ID, r.Action, r.BackupID = "restore-s3", "restore", "s3-import"
	if err = f.engine.performBackup(ctx, r, io.Discard); err != nil || f.value() != "before" {
		t.Fatal("configured S3 dump could not restore", err)
	}
}
