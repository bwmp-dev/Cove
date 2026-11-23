package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const zoneID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const accountID = "cccccccccccccccccccccccccccccccc"

type credentials struct{ AccountTag, TunnelSecret, TunnelID string }
type tunnel struct {
	ID      string `json:"id"`
	Account string `json:"account_tag"`
	Name    string `json:"name"`
	Source  string `json:"config_src"`
	Secret  string `json:"-"`
	Peer    string `json:"-"`
}
type record struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Comment string `json:"comment"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}
type provider struct {
	mu                                       sync.Mutex
	token                                    string
	tunnels                                  map[string]*tunnel
	records                                  map[string]record
	creates, deletes, dnsCreates, dnsDeletes int
}

func randomID() string {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		panic(err)
	}
	return hex.EncodeToString(data)
}
func result(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"success": true, "result": value, "result_info": map[string]int{"total_pages": 1}})
}

func (p *provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(200)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.URL.Path == "/fixture/tunnel-auth" && r.Method == "POST" {
		var credential credentials
		if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&credential) != nil {
			w.WriteHeader(400)
			return
		}
		owned := p.tunnels[credential.TunnelID]
		if owned == nil || credential.AccountTag != accountID || subtle.ConstantTimeCompare([]byte(owned.Secret), []byte(credential.TunnelSecret)) != 1 {
			w.WriteHeader(403)
			return
		}
		peer, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		owned.Peer = peer
		w.WriteHeader(200)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+p.token)) != 1 {
		w.WriteHeader(401)
		return
	}
	if r.URL.Path == "/fixture/state" {
		result(w, map[string]int{"tunnels": len(p.tunnels), "records": len(p.records), "creates": p.creates, "deletes": p.deletes, "dns_creates": p.dnsCreates, "dns_deletes": p.dnsDeletes})
		return
	}
	if r.URL.Path == "/fixture/forward" && r.Method == "POST" {
		var input struct {
			Hostname, Path string
			Headers        map[string]string
		}
		if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&input) != nil || !strings.HasPrefix(input.Path, "/") || !strings.HasSuffix(input.Hostname, ".example.test") {
			w.WriteHeader(400)
			return
		}
		var peer string
		for _, record := range p.records {
			if record.Name == input.Hostname {
				if owned := p.tunnels[strings.TrimSuffix(record.Content, ".cfargotunnel.com")]; owned != nil {
					peer = owned.Peer
				}
			}
		}
		if peer == "" {
			w.WriteHeader(503)
			return
		}
		request, err := http.NewRequest("GET", "http://"+net.JoinHostPort(peer, "18080")+input.Path, nil)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		request.Host = input.Hostname
		for name, value := range input.Headers {
			request.Header.Set(name, value)
		}
		response, err := (&http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}).Do(request)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer response.Body.Close()
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		io.Copy(w, io.LimitReader(response.Body, 65536))
		return
	}
	zones := "/client/v4/zones"
	if r.Method == "GET" && r.URL.Path == zones {
		result(w, []map[string]any{{"id": zoneID, "name": "example.test", "account": map[string]string{"id": accountID}}})
		return
	}
	if r.Method == "GET" && r.URL.Path == zones+"/"+zoneID {
		result(w, map[string]any{"id": zoneID, "name": "example.test", "account": map[string]string{"id": accountID}})
		return
	}
	base := "/client/v4/accounts/" + accountID + "/cfd_tunnel"
	if r.URL.Path == base {
		if r.Method == "GET" {
			list := []*tunnel{}
			for _, owned := range p.tunnels {
				if r.URL.Query().Get("name") == "" || owned.Name == r.URL.Query().Get("name") {
					list = append(list, owned)
				}
			}
			result(w, list)
			return
		}
		if r.Method == "POST" {
			var body struct{ Name, ConfigSrc, TunnelSecret string }
			var raw map[string]string
			if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&raw) != nil {
				w.WriteHeader(400)
				return
			}
			body.Name, body.ConfigSrc, body.TunnelSecret = raw["name"], raw["config_src"], raw["tunnel_secret"]
			if !strings.HasPrefix(body.Name, "cove-") || body.ConfigSrc != "local" || len(body.TunnelSecret) != 44 {
				w.WriteHeader(400)
				return
			}
			id := randomID()
			id = id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
			owned := &tunnel{ID: id, Name: body.Name, Source: "local", Account: accountID, Secret: body.TunnelSecret}
			p.tunnels[id] = owned
			p.creates++
			result(w, owned)
			return
		}
	}
	if strings.HasPrefix(r.URL.Path, base+"/") {
		id := strings.TrimPrefix(r.URL.Path, base+"/")
		owned := p.tunnels[id]
		if owned == nil {
			w.WriteHeader(404)
			return
		}
		if r.Method == "GET" {
			result(w, owned)
			return
		}
		if r.Method == "DELETE" {
			delete(p.tunnels, id)
			p.deletes++
			result(w, map[string]string{"id": id})
			return
		}
	}
	dns := zones + "/" + zoneID + "/dns_records"
	if r.URL.Path == dns {
		if r.Method == "GET" {
			list := []record{}
			for _, record := range p.records {
				if r.URL.Query().Get("name") == "" || record.Name == r.URL.Query().Get("name") {
					list = append(list, record)
				}
			}
			result(w, list)
			return
		}
		if r.Method == "POST" {
			var item record
			if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&item) != nil || item.Type != "CNAME" || !item.Proxied || !strings.HasSuffix(item.Name, ".example.test") || !strings.HasPrefix(item.Comment, "cove-tunnel-") {
				w.WriteHeader(400)
				return
			}
			for _, old := range p.records {
				if old.Name == item.Name {
					w.WriteHeader(409)
					return
				}
			}
			item.ID = randomID()
			p.records[item.ID] = item
			p.dnsCreates++
			result(w, item)
			return
		}
	}
	if strings.HasPrefix(r.URL.Path, dns+"/") {
		id := strings.TrimPrefix(r.URL.Path, dns+"/")
		item, ok := p.records[id]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if r.Method == "GET" {
			result(w, item)
			return
		}
		if r.Method == "DELETE" {
			delete(p.records, id)
			p.dnsDeletes++
			result(w, map[string]string{"id": id})
			return
		}
	}
	w.WriteHeader(400)
}

func connector() {
	public, err := os.ReadFile("/fake-edge/cert.pem")
	if err != nil {
		log.Fatal("fixture edge trust missing")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(public) {
		log.Fatal("fixture edge trust invalid")
	}
	credential, err := os.ReadFile("/config/credentials.json")
	if err != nil {
		log.Fatal("fixture credential missing")
	}
	var config struct {
		Ingress []struct {
			Hostname, Service string
			Origin            struct {
				ServerName string `json:"originServerName"`
				CAPool     string `json:"caPool"`
				Host       string `json:"httpHostHeader"`
			} `json:"originRequest"`
		}
	}
	data, err := os.ReadFile("/config/config.json")
	if err != nil || json.Unmarshal(data, &config) != nil {
		log.Fatal("fixture connector configuration invalid")
	}
	var ready atomic.Bool
	auth := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "api.cloudflare.com", MinVersion: tls.VersionTLS13}}}
	go func() {
		for {
			response, err := auth.Post(os.Getenv("COVE_TUNNEL_FIXTURE_PROVIDER")+"/fixture/tunnel-auth", "application/json", bytes.NewReader(credential))
			ok := false
			if err == nil {
				ok = response.StatusCode == 200
				response.Body.Close()
			}
			ready.Store(ok)
			time.Sleep(time.Second)
		}
	}()
	origins := map[string]*httputil.ReverseProxy{}
	for _, entry := range config.Ingress {
		if entry.Hostname == "" {
			continue
		}
		target, err := url.Parse(entry.Service)
		if err != nil {
			log.Fatal("fixture origin URL invalid")
		}
		root, err := os.ReadFile(entry.Origin.CAPool)
		if err != nil {
			log.Fatal("fixture origin root missing")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(root) {
			log.Fatal("fixture origin root invalid")
		}
		host := entry.Origin.Host
		origins[entry.Hostname] = &httputil.ReverseProxy{Rewrite: func(request *httputil.ProxyRequest) { request.SetURL(target); request.Out.Host = host }, Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: entry.Origin.ServerName, MinVersion: tls.VersionTLS13}}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(502) }}
	}
	go func() {
		log.Fatal((&http.Server{Addr: "127.0.0.1:20241", ReadHeaderTimeout: 3 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/ready" || !ready.Load() {
				w.WriteHeader(503)
				return
			}
			w.WriteHeader(200)
		})}).ListenAndServe())
	}()
	log.Fatal((&http.Server{Addr: ":18080", ReadHeaderTimeout: 3 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := origins[r.Host]; origin != nil {
			origin.ServeHTTP(w, r)
		} else {
			w.WriteHeader(404)
		}
	})}).ListenAndServe())
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "fixture-stream" {
		connection, err := net.DialTimeout("tcp", "127.0.0.1:443", 5*time.Second)
		if err != nil {
			os.Exit(1)
		}
		defer connection.Close()
		go func() {
			io.Copy(connection, os.Stdin)
			connection.(*net.TCPConn).CloseWrite()
		}()
		io.Copy(os.Stdout, connection)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "tunnel" {
		connector()
		return
	}
	token := os.Getenv("COVE_TUNNEL_FIXTURE_TOKEN")
	if len(token) < 32 {
		log.Fatal("an isolated generated fixture token is required")
	}
	p := &provider{token: token, tunnels: map[string]*tunnel{}, records: map[string]record{}}
	log.Fatal((&http.Server{Addr: ":443", ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, Handler: p}).ListenAndServeTLS("/fixture/cert.pem", "/fixture/key.pem"))
}
