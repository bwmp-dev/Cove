package worker

import (
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
)

var errPITRQuota = errors.New("PITR archive admission limit reached")

type pitrQuota struct {
	mu          sync.Mutex
	limit       int64
	bytes       int64
	pending     int64
	objects     int
	transferred int64
	created     int
	reached     chan struct{}
}

type pitrQuotaEpoch struct {
	transferred int64
	created     int
}

func newPITRQuota(limit int64, inventory pitrInventory) *pitrQuota {
	return &pitrQuota{limit: limit, bytes: inventory.Bytes, objects: inventory.Objects, reached: make(chan struct{}, 1)}
}

func (quota *pitrQuota) epoch() pitrQuotaEpoch {
	quota.mu.Lock()
	defer quota.mu.Unlock()
	return pitrQuotaEpoch{quota.transferred, quota.created}
}

func (quota *pitrQuota) reconcile(epoch pitrQuotaEpoch, inventory pitrInventory) {
	quota.mu.Lock()
	defer quota.mu.Unlock()
	// Writes completed while listing may already be present in the result; count them again to avoid undercharging a changing archive.
	quota.bytes = inventory.Bytes + quota.transferred - epoch.transferred
	quota.objects = inventory.Objects + quota.created - epoch.created
}

func (quota *pitrQuota) signal() {
	select {
	case quota.reached <- struct{}{}:
	default:
	}
}

func (quota *pitrQuota) addObject() error {
	quota.mu.Lock()
	defer quota.mu.Unlock()
	if quota.objects >= pitrObjectLimit {
		quota.signal()
		return errPITRQuota
	}
	quota.objects++
	quota.created++
	return nil
}

type pitrQuotaReader struct {
	source io.ReadCloser
	quota  *pitrQuota
}

func (reader *pitrQuotaReader) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	reader.quota.mu.Lock()
	remaining := reader.quota.limit - reader.quota.bytes - reader.quota.pending
	if remaining <= 0 {
		reader.quota.signal()
		reader.quota.mu.Unlock()
		return 0, errPITRQuota
	}
	if int64(len(buffer)) > remaining {
		buffer = buffer[:int(remaining)]
	}
	reader.quota.pending += int64(len(buffer))
	reader.quota.mu.Unlock()
	n, err := reader.source.Read(buffer)
	reader.quota.mu.Lock()
	reader.quota.pending -= int64(len(buffer))
	reader.quota.bytes += int64(n)
	reader.quota.transferred += int64(n)
	reader.quota.mu.Unlock()
	return n, err
}

func (reader *pitrQuotaReader) Close() error { return reader.source.Close() }

func writePITRS3Error(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if status == http.StatusInsufficientStorage {
		io.WriteString(w, "<Error><Code>InsufficientStorage</Code><Message>PITR archive admission limit reached</Message></Error>")
	} else {
		io.WriteString(w, "<Error><Code>AccessDenied</Code><Message>Unsupported PITR archive request</Message></Error>")
	}
}

func pitrQuotaProxy(target *url.URL, archive string, quota *pitrQuota) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		if errors.Is(err, errPITRQuota) {
			writePITRS3Error(w, http.StatusInsufficientStorage)
			return
		}
		http.Error(w, "PITR storage bridge is unavailable", http.StatusBadGateway)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			writePITRS3Error(w, http.StatusForbidden)
			return
		}
		write := r.Method == http.MethodPut || r.Method == http.MethodPost && r.URL.Query().Has("uploads") || r.Method == http.MethodPost && r.URL.Query().Has("uploadId")
		if write {
			if r.URL.Path != "/"+archive && !strings.HasPrefix(r.URL.Path, "/"+archive+"/") {
				writePITRS3Error(w, http.StatusForbidden)
				return
			}
			if err := quota.addObject(); err != nil {
				writePITRS3Error(w, http.StatusInsufficientStorage)
				return
			}
			if r.Method == http.MethodPut && r.Body != nil && r.Body != http.NoBody {
				r.Body = &pitrQuotaReader{source: r.Body, quota: quota}
			}
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, pitrMetadataLimit)
		}
		proxy.ServeHTTP(w, r)
	})
}
