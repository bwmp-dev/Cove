package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const baseDigest = "sha256:d45c89d80e13d7ad0fd555b5130f22a866d9dd10e861f589932303ef2314c7de"

var digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func mirror(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	if r.URL.Path == "/fixture/network-sentinel" {
		fmt.Println("fixture direct network sentinel", r.Method)
		fmt.Fprint(w, "owned-nixpacks-network")
		return
	}
	if r.URL.Path == "/v2/" {
		w.WriteHeader(http.StatusOK)
		return
	}
	var resource string
	for _, prefix := range []string{"/v2/railwayapp/nixpacks/manifests/", "/v2/railwayapp/nixpacks/blobs/"} {
		if strings.HasPrefix(r.URL.Path, prefix) {
			resource = strings.TrimPrefix(r.URL.Path, prefix)
		}
	}
	if resource == "ubuntu-1745885067" {
		resource = baseDigest
	}
	if !digest.MatchString(resource) {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(filepath.Join("/fixture/base", resource[7:]))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != resource {
		http.Error(w, "fixture digest mismatch", 500)
		return
	}
	if _, err := file.Seek(0, 0); err != nil {
		http.Error(w, "fixture read failed", 500)
		return
	}
	w.Header().Set("Docker-Content-Digest", resource)
	if strings.Contains(r.URL.Path, "/manifests/") {
		var metadata struct {
			MediaType string `json:"mediaType"`
		}
		if err := json.NewDecoder(io.LimitReader(file, 8<<20)).Decode(&metadata); err != nil || metadata.MediaType == "" {
			http.Error(w, "fixture manifest invalid", 500)
			return
		}
		w.Header().Set("Content-Type", metadata.MediaType)
		if _, err := file.Seek(0, 0); err != nil {
			http.Error(w, "fixture read failed", 500)
			return
		}
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	fmt.Println("fixture OCI read", r.Method, r.URL.Path)
	http.ServeContent(w, r, resource, time.Time{}, file)
}

func main() {
	if len(os.Args) == 4 && os.Args[1] == "network-check" {
		client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
		response, err := client.Get(os.Args[3])
		if response != nil {
			defer response.Body.Close()
		}
		if os.Args[2] == "none" {
			if err == nil {
				panic("network-none build reached the owned sentinel")
			}
		} else {
			if err != nil {
				panic("owned sentinel was not reachable for the bypass control")
			}
			data, err := io.ReadAll(io.LimitReader(response.Body, 1024))
			if err != nil || string(data) != "owned-nixpacks-network" {
				panic("unexpected owned sentinel response")
			}
		}
		if err := os.WriteFile("/app/network-proof", []byte(os.Args[2]), 0644); err != nil {
			panic(err)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "mirror" {
		server := &http.Server{Addr: ":16000", Handler: http.HandlerFunc(mirror), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
		if err := server.ListenAndServe(); err != nil {
			panic(err)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "build-secret-check" {
		data, err := os.ReadFile("/run/secrets/TOKEN")
		sum := sha256.Sum256(data)
		if err != nil || hex.EncodeToString(sum[:]) != os.Args[2] {
			panic("selected fixture secret unavailable")
		}
		if _, err := os.Lstat("/run/secrets/UNSELECTED"); !os.IsNotExist(err) {
			panic("unselected fixture secret became available")
		}
		if err := os.WriteFile("/app/build-proof", []byte("selected-secret-read"), 0644); err != nil {
			panic(err)
		}
		return
	}
	proof := "selected-secret-read"
	if len(os.Args) == 2 && os.Args[1] == "browser" {
		proof = "offline-build-completed"
	}
	server := &http.Server{Addr: ":8080", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile("/app/build-proof")
		if err != nil || string(data) != proof {
			http.Error(w, "fixture build proof missing", 500)
			return
		}
		fmt.Fprint(w, "nixpacks-offline-fixture")
	}), ReadHeaderTimeout: 5 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		panic(err)
	}
}
