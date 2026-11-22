package control

import (
	"cove/internal/buildinfo"
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
	if r.Method == http.MethodPut {
		var in struct {
			Channel string `json:"channel"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.Channel != "beta" && in.Channel != "release" {
			http.Error(w, "choose beta or release", http.StatusBadRequest)
			return
		}
		if _, err := s.DB.Exec(r.Context(), "UPDATE control_settings SET update_channel=$1 WHERE id=1", in.Channel); err != nil {
			http.Error(w, "could not save update channel", http.StatusInternalServerError)
			return
		}
	}
	var channel string
	if err := s.DB.QueryRow(r.Context(), "SELECT update_channel FROM control_settings WHERE id=1").Scan(&channel); err != nil {
		http.Error(w, "update settings unavailable", http.StatusServiceUnavailable)
		return
	}
	respond(w, http.StatusOK, map[string]string{"channel": channel, "version": buildinfo.Version, "revision": buildinfo.Revision, "build_channel": buildinfo.Channel})
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
