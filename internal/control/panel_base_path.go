package control

import (
	"net/http"
	"net/url"
	"strings"
)

func (s *Server) panelBasePath() string {
	u, err := url.Parse(s.PublicURL)
	if err != nil {
		return ""
	}
	return u.Path
}
func (s *Server) panelOrigin() string {
	u, err := url.Parse(s.PublicURL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

type panelPathResponse struct {
	http.ResponseWriter
	base string
}

func (w *panelPathResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *panelPathResponse) WriteHeader(status int) {
	location := w.Header().Get("Location")
	if strings.HasPrefix(location, "/") && !strings.HasPrefix(location, "//") {
		w.Header().Set("Location", w.base+location)
	}
	w.ResponseWriter.WriteHeader(status)
}
func (s *Server) mountPanel(next http.Handler) http.Handler {
	base := s.panelBasePath()
	if base == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == base {
			u := url.URL{Path: base + "/", RawQuery: r.URL.RawQuery}
			http.Redirect(w, r, u.String(), http.StatusPermanentRedirect)
			return
		}
		// Container and reverse-proxy liveness checks remain path-independent.
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if !strings.HasPrefix(r.URL.Path, base+"/") {
			http.NotFound(w, r)
			return
		}
		sub := r.Clone(r.Context())
		u := *r.URL
		sub.URL = &u
		sub.URL.Path = strings.TrimPrefix(r.URL.Path, base)
		sub.URL.RawPath = ""
		next.ServeHTTP(&panelPathResponse{ResponseWriter: w, base: base}, sub)
	})
}
