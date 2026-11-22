package control

import (
	"html"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

func (s *Server) frontend(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/") || strings.HasPrefix(r.URL.Path, "/bootstrap/") {
		http.NotFound(w, r)
		return
	}
	directory := s.FrontendDir
	if directory == "" {
		directory = "frontend/dist"
	}
	files := os.DirFS(directory)
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	info, err := fs.Stat(files, name)
	if err != nil || info.IsDir() {
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		name = "index.html"
	}
	if _, err = fs.Stat(files, name); err != nil {
		http.Error(w, "Dashboard build is unavailable.", 503)
		return
	}
	nonce := randomToken()
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'nonce-"+nonce+"'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self' https://github.com")
	w.Header().Set("Referrer-Policy", "same-origin")
	if strings.HasPrefix(r.URL.Path, "/shared/") {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	}
	w.Header().Set("X-Frame-Options", "DENY")
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if name == "index.html" {
		data, err := fs.ReadFile(files, name)
		if err != nil {
			http.Error(w, "Dashboard build is unavailable.", 503)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		base := html.EscapeString(s.panelBasePath())
		_, _ = w.Write([]byte(strings.Replace(string(data), "<head>", "<head><base href=\""+base+"/\"><meta name=\"cove-base-path\" content=\""+base+"\"><meta name=\"cove-style-nonce\" content=\""+html.EscapeString(nonce)+"\">", 1)))
		return
	}
	http.ServeFileFS(w, r, files, name)
}
