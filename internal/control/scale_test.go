package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInventoryScaleIntegration(t *testing.T) {
	database := os.Getenv("COVE_TEST_DATABASE_URL")
	if database == "" {
		t.Skip("COVE_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db := openTestDatabase(t, database)
	s := &Server{DB: db, Key: bytes.Repeat([]byte{4}, 32), AdminToken: strings.Repeat("s", 32), PublicURL: "https://cove.example.com"}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO nodes(id,name,address,port,public_key,private_key) SELECT 'node-'||n,'Node '||n,'192.0.2.1',22,'public',decode('00','hex') FROM generate_series(1,2048) n`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO projects(id,name,team_id) VALUES('p','Scale','default')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO workloads(id,node_id,name,configuration,project_id) SELECT 'app-'||n,'node-'||n,'Application '||n,decode('00','hex'),'p' FROM generate_series(1,2048) n`); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	start := time.Now()
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Go(func() {
			for _, path := range []string{"/api/nodes", "/api/workloads"} {
				r := httptest.NewRequest("GET", path, nil)
				r.Header.Set("Authorization", "Bearer "+s.AdminToken)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Errorf("%s: %d", path, w.Code)
					return
				}
				var items []json.RawMessage
				if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil || len(items) != 2048 {
					t.Errorf("%s: count %d, %v", path, len(items), err)
				}
				if strings.Contains(w.Body.String(), "private_key") || strings.Contains(w.Body.String(), "configuration") {
					t.Errorf("%s exposed private fields", path)
				}
			}
		})
	}
	group.Wait()
	t.Logf("40 inventory reads from 20 concurrent clients over 2048 synthetic nodes/workloads completed in %s", time.Since(start))
}
