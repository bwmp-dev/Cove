package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func certificate() (tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Cove local recipe fixture"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile("/state/ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		return tls.Certificate{}, err
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "ghcr.io"}, DNSNames: []string{"ghcr.io"}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func artifact(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "read only", 405)
		return
	}
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	if r.URL.Path == "/v2/" {
		_, _ = io.WriteString(w, "{}")
		return
	}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) != 6 || parts[1] != "v2" || parts[2] != "railwayapp" || !strings.HasPrefix(parts[3], "railpack-") || parts[4] != "manifests" && parts[4] != "blobs" || !digestPattern.MatchString(parts[5]) {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join("/artifacts/blobs/sha256", strings.TrimPrefix(parts[5], "sha256:"))
	file, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	if parts[4] == "manifests" {
		data, err := io.ReadAll(io.LimitReader(file, 2<<20))
		if err != nil {
			http.Error(w, "unavailable", 500)
			return
		}
		sum := sha256.Sum256(data)
		if "sha256:"+hex.EncodeToString(sum[:]) != parts[5] {
			http.Error(w, "artifact hash mismatch", 500)
			return
		}
		var manifest struct {
			MediaType string `json:"mediaType"`
		}
		if json.Unmarshal(data, &manifest) != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", manifest.MediaType)
		w.Header().Set("Docker-Content-Digest", parts[5])
		http.ServeContent(w, r, "manifest", time.Time{}, strings.NewReader(string(data)))
		return
	}
	info, err := file.Stat()
	if err != nil {
		http.Error(w, "unavailable", 500)
		return
	}
	w.Header().Set("Docker-Content-Digest", parts[5])
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func dns() error {
	connection, err := net.ListenPacket("udp", ":53")
	if err != nil {
		return err
	}
	defer connection.Close()
	buffer := make([]byte, 4096)
	for {
		n, address, err := connection.ReadFrom(buffer)
		if err != nil {
			return err
		}
		query := append([]byte{}, buffer[:n]...)
		if n < 17 || binary.BigEndian.Uint16(query[4:6]) != 1 {
			continue
		}
		position := 12
		for position < n && query[position] != 0 {
			size := int(query[position])
			position += size + 1
		}
		if position+5 > n {
			continue
		}
		position++
		kind := binary.BigEndian.Uint16(query[position : position+2])
		end := position + 4
		response := append([]byte{}, query[:end]...)
		response[2], response[3] = 0x81, 0x80
		response[6], response[7], response[8], response[9], response[10], response[11] = 0, 0, 0, 0, 0, 0
		if kind == 1 {
			response[7] = 1
			response = append(response, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 1, 0, 4, 1, 1, 1, 1)
		}
		_, _ = connection.WriteTo(response, address)
	}
}

func main() {
	if os.Getenv("COVE_RECIPE_FIXTURE") != "local-only" {
		panic("fixture-only executable")
	}
	if len(os.Args) == 2 && os.Args[1] == "metadata" {
		server := &http.Server{Addr: "169.254.169.254:80", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "owned-metadata-fixture") }), ReadHeaderTimeout: 5 * time.Second}
		if err := server.ListenAndServe(); err != nil {
			panic(err)
		}
		return
	}
	if err := os.MkdirAll("/state", 0700); err != nil {
		panic(err)
	}
	cert, err := certificate()
	if err != nil {
		panic(err)
	}
	var mutex sync.Mutex
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		file, err := os.OpenFile("/state/proxy.jsonl", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err == nil {
			err = json.NewEncoder(file).Encode(map[string]string{"method": r.Method, "host": r.Host})
			err = errors.Join(err, file.Close())
		}
		mutex.Unlock()
		if err != nil {
			http.Error(w, "fixture recording failed", 500)
			return
		}
		http.Error(w, "owned metadata sinkhole: no external network", 502)
	})
	go func() {
		if err := dns(); err != nil {
			panic(err)
		}
	}()
	go func() {
		server := &http.Server{Addr: ":3129", Handler: proxy, ReadHeaderTimeout: 5 * time.Second}
		if err := server.ListenAndServe(); err != nil {
			panic(err)
		}
	}()
	server := &http.Server{Addr: ":443", Handler: http.HandlerFunc(artifact), ReadHeaderTimeout: 5 * time.Second, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	if err := server.ListenAndServeTLS("", ""); err != nil {
		panic(err)
	}
}
