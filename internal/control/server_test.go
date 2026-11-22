package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEncryption(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	plain := []byte("private identity")
	encrypted, err := seal(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unseal(key, encrypted)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("round trip failed")
	}
	encrypted[len(encrypted)-1] ^= 1
	if _, err := unseal(key, encrypted); err == nil {
		t.Fatal("tampering accepted")
	}
	if _, err := unseal(key, []byte{1}); err == nil {
		t.Fatal("truncation accepted")
	}
}

func TestSSHHostPinning(t *testing.T) {
	pub, seed, err := keyPair()
	if err != nil {
		t.Fatal(err)
	}
	authorized, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(pub))
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	host, _ := ssh.NewSignerFromKey(private)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), authorized.Marshal()) {
			return nil, os.ErrPermission
		}
		return nil, nil
	}}
	config.AddHostKey(host)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2; i++ {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			c, ch, req, err := ssh.NewServerConn(conn, config)
			if err != nil {
				conn.Close()
				continue
			}
			go ssh.DiscardRequests(req)
			for next := range ch {
				channel, requests, err := next.Accept()
				if err != nil {
					continue
				}
				go func() {
					defer channel.Close()
					for r := range requests {
						if r.Type == "exec" {
							r.Reply(true, nil)
							io.Copy(io.Discard, channel)
							channel.Write([]byte(`{"probe":{"protocol":1,"os":"linux","arch":"amd64","docker":"test","compose":"test","ready":true}}`))
							channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
							return
						}
						r.Reply(false, nil)
					}
				}()
			}
			c.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	p, err := probeSSH(context.Background(), "127.0.0.1", port, string(ssh.MarshalAuthorizedKey(host.PublicKey())), seed)
	if err != nil || !p.Ready {
		t.Fatalf("valid host rejected: %v", err)
	}
	wrong, _, _ := keyPair()
	if _, err := probeSSH(context.Background(), "127.0.0.1", port, wrong, seed); err == nil {
		t.Fatal("changed host key accepted")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SSH fixture did not finish")
	}
}

func TestEnrollmentIntegration(t *testing.T) {
	database := os.Getenv("COVE_TEST_DATABASE_URL")
	if database == "" {
		t.Skip("COVE_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db := openTestDatabase(t, database)
	var err error
	s := &Server{DB: db, Key: bytes.Repeat([]byte{8}, 32), AdminToken: strings.Repeat("a", 32), PublicURL: "https://cove.example.com", BinaryDir: t.TempDir()}
	if err = s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if err = os.WriteFile(s.BinaryDir+"/cove-worker-linux-"+arch, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	handler := s.Handler()
	call := func(method, path, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if got := call("GET", "/api/nodes", "", "").Code; got != 401 {
		t.Fatalf("unauthorized list: %d", got)
	}
	create := call("POST", "/api/nodes", s.AdminToken, `{"name":"fixture","address":"127.0.0.1","port":2222}`)
	if create.Code != 201 {
		t.Fatal(create.Body.String())
	}
	var result struct {
		ID      string `json:"id"`
		Command string `json:"setup_command"`
	}
	json.Unmarshal(create.Body.Bytes(), &result)
	defer func() {
		db.Exec(ctx, "DELETE FROM enrollments WHERE node_id=$1", result.ID)
		db.Exec(ctx, "DELETE FROM nodes WHERE id=$1", result.ID)
	}()
	token := strings.Split(strings.Split(result.Command, "Bearer ")[1], "'")[0]
	if w := call("GET", "/install.sh", token, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "restrict,command=") {
		t.Fatal("installer missing restricted identity")
	}
	pub, _, _ := keyPair()
	body, _ := json.Marshal(map[string]string{"host_key": pub})
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Go(func() { codes <- call("POST", "/bootstrap/register", token, string(body)).Code })
	}
	wg.Wait()
	close(codes)
	successes, denials := 0, 0
	for code := range codes {
		if code == 200 {
			successes++
		}
		if code == 401 {
			denials++
		}
	}
	if successes != 1 || denials != 1 {
		t.Fatal("token was not consumed exactly once")
	}
	if w := call("GET", "/install.sh", token, ""); w.Code != 401 {
		t.Fatal("consumed token served installer")
	}
	listing := call("GET", "/api/nodes", s.AdminToken, "")
	if strings.Contains(listing.Body.String(), token) || strings.Contains(listing.Body.String(), "private_key") {
		t.Fatal("list leaked credentials")
	}
	expired := randomToken()
	if _, err = db.Exec(ctx, "INSERT INTO enrollments(token_hash,node_id,expires_at) VALUES($1,$2,now()-interval '1 minute')", tokenHash(expired), result.ID); err != nil {
		t.Fatal(err)
	}
	if w := call("POST", "/bootstrap/register", expired, string(body)); w.Code != 401 {
		t.Fatal("expired token accepted")
	}
}
