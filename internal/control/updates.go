package control

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"text/template"
)

func (s *Server) workerUpdateInstaller(w http.ResponseWriter, r *http.Request) {
	t, err := template.ParseFS(assets, "worker-update-install.sh.tmpl", "worker-update.sh.tmpl")
	if err != nil {
		http.Error(w, "updater installer unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript")
	_ = t.Execute(w, map[string]string{"URL": s.PublicURL})
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	s.governedUpdateSettings(w, r)
}

// Release artifacts are public; enrollment and node RPC credentials remain separate.
func (s *Server) workerRelease(w http.ResponseWriter, r *http.Request) {
	arch := r.PathValue("arch")
	if arch != "amd64" && arch != "arm64" {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(filepath.Join(s.BinaryDir, "cove-worker-linux-"+arch))
	if err != nil {
		http.Error(w, "worker release unavailable", http.StatusServiceUnavailable)
		return
	}
	sum := sha256.Sum256(data)
	checksum := hex.EncodeToString(sum[:])
	if r.PathValue("checksum") == "" {
		respond(w, http.StatusOK, map[string]string{"sha256": checksum})
		return
	}
	if r.PathValue("checksum") != checksum {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

func (s *Server) workerSignature(w http.ResponseWriter, r *http.Request) {
	arch := r.PathValue("arch")
	if arch != "amd64" && arch != "arm64" {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(filepath.Join(s.BinaryDir, "cove-worker-linux-"+arch))
	if err != nil {
		http.Error(w, "worker release unavailable", 503)
		return
	}
	sum := sha256.Sum256(data)
	if r.PathValue("checksum") != hex.EncodeToString(sum[:]) {
		http.NotFound(w, r)
		return
	}
	bundle, err := os.ReadFile(filepath.Join(s.BinaryDir, "cove-worker-linux-"+arch+".sigstore.json"))
	if err != nil {
		http.Error(w, "signed worker release unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(bundle)
}
