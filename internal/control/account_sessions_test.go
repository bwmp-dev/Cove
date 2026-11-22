package control

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAccountSessionsArePrivateAndRevocable(t *testing.T) {
	f := newAccountFixture(t)
	owner := f.user("session-owner", "session-owner@example.com")
	other := f.user("session-other", "session-other@example.com")
	f.sql("INSERT INTO team_members(team_id,user_id,role,grants) VALUES('default','session-owner','member','[]'),('default','session-other','member','[]')")
	second := randomToken()
	f.sql("INSERT INTO sessions(token_hash,user_id,expires_at,user_agent) VALUES($1,'session-owner',now()+interval '1 hour','Test browser')", tokenHash(second))
	f.sql("INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,'session-owner',now()-interval '1 hour')", tokenHash(randomToken()))
	var sessions []struct {
		ID      string `json:"id"`
		Current bool   `json:"current"`
	}
	response := f.call(owner, "default", "GET", "/api/account/sessions", nil, 200)
	if err := json.Unmarshal(response.Body.Bytes(), &sessions); err != nil || len(sessions) != 2 {
		t.Fatal(response.Body.String(), err)
	}
	for _, session := range sessions {
		f.call(other, "default", "DELETE", "/api/account/sessions/"+session.ID, nil, 404)
		if session.Current {
			f.call(owner, "default", "DELETE", "/api/account/sessions/"+session.ID, nil, 404)
		} else {
			f.call(owner, "default", "DELETE", "/api/account/sessions/"+session.ID, nil, 200)
		}
	}
	f.call(second, "default", "GET", "/api/session", nil, 401)
	f.call(owner, "default", "GET", "/api/session", nil, 200)
	var count int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM sessions WHERE user_id='session-other'").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
