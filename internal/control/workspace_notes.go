package control

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

func renderNotes(markdown string) (string, error) {
	source := []byte(markdown)
	md := goldmark.New()
	document := md.Parser().Parse(text.NewReader(source))
	var images []ast.Node
	if err := ast.Walk(document, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && n.Kind() == ast.KindImage {
			images = append(images, n)
		}
		return ast.WalkContinue, nil
	}); err != nil {
		return "", err
	}
	// Notes must not make third-party requests when somebody opens a resource.
	for _, image := range images {
		image.Parent().RemoveChild(image.Parent(), image)
	}
	var output bytes.Buffer
	if err := md.Renderer().Render(&output, source, document); err != nil {
		return "", err
	}
	return output.String(), nil
}
func (s *Server) noteVersion(raw []byte) string {
	mac := hmac.New(sha256.New, s.Key)
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}
func (s *Server) workspaceNotes(w http.ResponseWriter, r *http.Request) {
	kind, table := "workload", "workload_notes"
	if strings.HasPrefix(r.URL.Path, "/api/projects/") {
		kind, table = "project", "project_notes"
	}
	level := 1
	if r.Method != "GET" {
		level = 3
	}
	id := r.PathValue("id")
	if !s.requireAccess(w, r, kind, id, level) {
		return
	}
	tx, err := s.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "notes unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	// Serialise first creation as well as edits of an existing note.
	if _, err = tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, table+":"+id); err != nil {
		http.Error(w, "notes unavailable", 503)
		return
	}
	var encrypted []byte
	var updated *time.Time
	err = tx.QueryRow(r.Context(), `SELECT content,updated_at FROM `+table+` WHERE resource_id=$1`, id).Scan(&encrypted, &updated)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "notes unavailable", 503)
		return
	}
	raw := []byte{}
	if len(encrypted) > 0 {
		raw, err = unseal(s.Key, encrypted)
		if err != nil {
			http.Error(w, "notes unavailable", 503)
			return
		}
	}
	if r.Method == "PUT" {
		var in struct {
			Markdown string `json:"markdown"`
			Version  string `json:"version"`
		}
		if !decode(w, r, &in) {
			return
		}
		if len(in.Markdown) > 65536 || !utf8.ValidString(in.Markdown) || strings.ContainsRune(in.Markdown, 0) {
			http.Error(w, "notes must be valid text up to 64 KiB", 400)
			return
		}
		if in.Version != s.noteVersion(raw) {
			http.Error(w, "notes changed; reload before saving", 409)
			return
		}
		raw = []byte(in.Markdown)
		encrypted, err = seal(s.Key, raw)
		if err != nil {
			http.Error(w, "could not protect notes", 503)
			return
		}
		if err = tx.QueryRow(r.Context(), `INSERT INTO `+table+`(resource_id,content) VALUES($1,$2) ON CONFLICT(resource_id) DO UPDATE SET content=EXCLUDED.content,updated_at=now() RETURNING updated_at`, id, encrypted).Scan(&updated); err != nil {
			http.Error(w, "could not save notes", 503)
			return
		}
	}
	html, err := renderNotes(string(raw))
	if err != nil {
		http.Error(w, "could not render notes", 500)
		return
	}
	if err = tx.Commit(r.Context()); err != nil {
		http.Error(w, "notes unavailable", 503)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, map[string]any{"markdown": string(raw), "html": html, "version": s.noteVersion(raw), "updated_at": updated})
}
