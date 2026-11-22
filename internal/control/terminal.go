package control

import (
	"cove/internal/worker"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strconv"
)

func (s *Server) terminalOpen(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Node    string `json:"node_id"`
		Columns uint16 `json:"columns"`
		Rows    uint16 `json:"rows"`
	}
	if !decode(w, r, &in) {
		return
	}
	var ready bool
	if err := s.DB.QueryRow(r.Context(), `SELECT state='ready' AND COALESCE((capabilities->>'terminal')::boolean,false) FROM nodes WHERE id=$1`, in.Node).Scan(&ready); err != nil || !ready {
		http.Error(w, "select a ready node with terminal support; upgrade and verify its worker if needed", 409)
		return
	}
	response, err := s.nodeRPC(r.Context(), in.Node, worker.RPCRequest{Method: "terminal_open", Terminal: &worker.TerminalRequest{Columns: in.Columns, Rows: in.Rows}})
	if err != nil || response.Terminal == nil {
		http.Error(w, "could not open node terminal", 502)
		return
	}
	id := response.Terminal.ID
	_, err = s.DB.Exec(r.Context(), `INSERT INTO terminal_sessions(id,node_id,user_id,team_id,expires_at) VALUES($1,$2,$3,$4,now()+interval '1 hour')`, id, in.Node, actor(r).ID, actor(r).Team)
	if err != nil {
		_, _ = s.nodeRPC(r.Context(), in.Node, worker.RPCRequest{Method: "terminal_close", Terminal: &worker.TerminalRequest{ID: id}})
		http.Error(w, "could not register terminal session", 503)
		return
	}
	respond(w, 201, response.Terminal)
}

func (s *Server) terminalSession(w http.ResponseWriter, r *http.Request) {
	var node string
	err := s.DB.QueryRow(r.Context(), `SELECT node_id FROM terminal_sessions WHERE id=$1 AND user_id=$2 AND team_id=$3 AND expires_at>now()`, r.PathValue("id"), actor(r).ID, actor(r).Team).Scan(&node)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "terminal unavailable", 503)
		return
	}
	in := worker.TerminalRequest{ID: r.PathValue("id")}
	method := "terminal_read"
	switch r.Method {
	case "POST":
		var body struct {
			Data    []byte `json:"data"`
			Columns uint16 `json:"columns"`
			Rows    uint16 `json:"rows"`
		}
		if !decode(w, r, &body) {
			return
		}
		in.Data = body.Data
		in.Columns = body.Columns
		in.Rows = body.Rows
		method = "terminal_write"
	case "DELETE":
		method = "terminal_close"
	default:
		in.Offset, err = strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		if err != nil || in.Offset < 0 {
			http.Error(w, "invalid terminal offset", 400)
			return
		}
	}
	response, err := s.nodeRPC(r.Context(), node, worker.RPCRequest{Method: method, Terminal: &in})
	if err != nil || response.Terminal == nil {
		http.Error(w, "terminal unavailable or expired; reconnect to open a new session", 502)
		return
	}
	if r.Method == "DELETE" {
		if _, err = s.DB.Exec(r.Context(), "DELETE FROM terminal_sessions WHERE id=$1", in.ID); err != nil {
			http.Error(w, "terminal closed but session cleanup failed", 503)
			return
		}
	}
	respond(w, 200, response.Terminal)
}
