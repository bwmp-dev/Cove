package worker

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestPITRQuotaProxyPreservesSignedHostAndBoundsUploads(t *testing.T) {
	var mu sync.Mutex
	var received int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "signed.archive.invalid" || r.Header.Get("Authorization") != "synthetic-signature" {
			t.Error("signed request was rewritten")
		}
		data, err := io.ReadAll(r.Body)
		mu.Lock()
		received += len(data)
		mu.Unlock()
		if err != nil {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(200)
	}))
	defer backend.Close()
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	quota := newPITRQuota(32, pitrInventory{})
	proxy := httptest.NewServer(pitrQuotaProxy(target, "archive", quota))
	defer proxy.Close()
	send := func(path string, body string) int {
		request, err := http.NewRequest(http.MethodPut, proxy.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Host = "signed.archive.invalid"
		request.Header.Set("Authorization", "synthetic-signature")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	if code := send("/archive/first", strings.Repeat("a", 20)); code != 200 {
		t.Fatal("normal upload failed", code)
	}
	if code := send("/archive/second", strings.Repeat("b", 20)); code < 400 {
		t.Fatal("over-cap upload succeeded", code)
	}
	mu.Lock()
	total := received
	mu.Unlock()
	if total > 32 {
		t.Fatal("backend received more bytes than admitted", total)
	}
	if code := send("/another/escape", "data"); code != 403 {
		t.Fatal("unrelated archive write accepted", code)
	}
}

type pitrBlockingReader struct{ started, proceed chan struct{} }

func (reader *pitrBlockingReader) Read(buffer []byte) (int, error) {
	close(reader.started)
	<-reader.proceed
	return copy(buffer, bytes.Repeat([]byte("a"), len(buffer))), nil
}
func (*pitrBlockingReader) Close() error { return nil }

func TestPITRQuotaReconciliationKeepsInflightReservations(t *testing.T) {
	quota := newPITRQuota(20, pitrInventory{})
	source := &pitrBlockingReader{started: make(chan struct{}), proceed: make(chan struct{})}
	reader := &pitrQuotaReader{source: source, quota: quota}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 16)
		if _, err := reader.Read(buffer); err != nil {
			t.Error(err)
		}
	}()
	<-source.started
	epoch := quota.epoch()
	quota.reconcile(epoch, pitrInventory{})
	second := &pitrQuotaReader{source: io.NopCloser(strings.NewReader("more-than-free-space")), quota: quota}
	buffer := make([]byte, 16)
	if n, err := second.Read(buffer); err != nil || n != 4 {
		t.Fatal("pending write was not reserved", n, err)
	}
	close(source.proceed)
	<-done
	if _, err := second.Read(buffer); err == nil {
		t.Fatal("quota admitted bytes after reaching its limit")
	}
}
