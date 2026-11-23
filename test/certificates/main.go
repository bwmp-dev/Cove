package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const zone = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type record struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Comment string `json:"comment"`
	TTL     int    `json:"ttl"`
}

type provider struct {
	mu      sync.Mutex
	token   string
	records map[string]record
	creates int
	deletes int
}

func (p *provider) publish(name string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	post := func(path string, body map[string]string) error {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		response, err := client.Post("http://challenge:8055"+path, "application/json", bytes.NewReader(raw))
		if err != nil {
			return errors.New("fixture DNS management unavailable")
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			return errors.New("fixture DNS management refused update")
		}
		return nil
	}
	if err := post("/clear-txt", map[string]string{"host": name + "."}); err != nil {
		return err
	}
	for _, item := range p.records {
		if item.Name == name {
			if err := post("/set-txt", map[string]string{"host": name + ".", "value": item.Content}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && r.URL.Path == "/healthz" {
		w.WriteHeader(200)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+p.token)) != 1 {
		http.Error(w, "fixture credential required", 401)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	result := func(value any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": value, "result_info": map[string]int{"total_pages": 1}})
	}
	if r.Method == "GET" && r.URL.Path == "/fixture/state" {
		result(map[string]int{"records": len(p.records), "creates": p.creates, "deletes": p.deletes})
		return
	}
	if r.Method == "GET" && r.URL.Path == "/client/v4/zones" {
		result([]map[string]any{{"id": zone, "name": "example.test", "account": map[string]string{"id": strings.Repeat("c", 32)}}})
		return
	}
	base := "/client/v4/zones/" + zone + "/dns_records"
	if r.Method == "GET" && r.URL.Path == base {
		items := []record{}
		for _, item := range p.records {
			if name := r.URL.Query().Get("name"); name != "" && item.Name != name {
				continue
			}
			if kind := r.URL.Query().Get("type"); kind != "" && item.Type != kind {
				continue
			}
			items = append(items, item)
		}
		result(items)
		return
	}
	if r.Method == "POST" && r.URL.Path == base {
		var item record
		if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&item) != nil || item.Type != "TXT" || !strings.HasPrefix(item.Name, "_acme-challenge.") || !strings.HasSuffix(item.Name, ".example.test") || item.Content == "" || !strings.HasPrefix(item.Comment, "cove-acme-") || item.TTL != 60 {
			http.Error(w, "unexpected fixture DNS write", 400)
			return
		}
		id := make([]byte, 16)
		if _, err := rand.Read(id); err != nil {
			http.Error(w, "fixture entropy unavailable", 503)
			return
		}
		item.ID = hex.EncodeToString(id)
		p.records[item.ID] = item
		if err := p.publish(item.Name); err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		p.creates++
		result(item)
		return
	}
	if r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, base+"/") {
		id := strings.TrimPrefix(r.URL.Path, base+"/")
		item, exists := p.records[id]
		if !exists {
			http.NotFound(w, r)
			return
		}
		if !strings.HasPrefix(item.Comment, "cove-acme-") {
			http.Error(w, "refusing foreign fixture record deletion", 409)
			return
		}
		delete(p.records, id)
		if err := p.publish(item.Name); err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		p.deletes++
		result(map[string]string{"id": id})
		return
	}
	http.Error(w, "unexpected certificate fixture request", 400)
}

func main() {
	token := os.Getenv("COVE_CERTIFICATE_FIXTURE_TOKEN")
	if len(token) < 32 {
		log.Fatal("an isolated fixture credential is required")
	}
	p := &provider{token: token, records: map[string]record{}}
	server := &http.Server{Addr: ":443", Handler: p, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 16384}
	log.Fatal(server.ListenAndServeTLS("/fixture/cert.pem", "/fixture/key.pem"))
}
