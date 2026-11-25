package worker

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

func staticSiteHandler(directory string) (*staticSite, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	return &staticSite{root: root}, nil
}

type staticSite struct{ root *os.Root }

func (site *staticSite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	for _, segment := range strings.Split(r.URL.Path, "/") {
		if strings.HasPrefix(segment, ".") || strings.ContainsAny(segment, "\\\x00") {
			http.NotFound(w, r)
			return
		}
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	} else if strings.HasSuffix(r.URL.Path, "/") {
		name += "/index.html"
	}
	file, err := site.root.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func ServeStaticSite(ctx context.Context) error {
	handler, err := staticSiteHandler("/srv")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errors.New("the static output directory is unavailable")
		}
		return err
	}
	defer handler.root.Close()
	server := &http.Server{Addr: ":8080", Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: time.Minute, MaxHeaderBytes: 32768}
	stop := context.AfterFunc(ctx, func() { server.Close() })
	defer stop()
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
