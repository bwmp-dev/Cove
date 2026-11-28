package control

import (
	"net/http"
	"strings"
	"time"
	_ "time/tzdata"
)

func validTimezone(value string, inherit bool) bool {
	if value == "" {
		return inherit
	}
	if value == "Local" || len(value) > 100 || strings.ContainsAny(value, "\\\x00\r\n") {
		return false
	}
	_, err := time.LoadLocation(value)
	return err == nil
}
func (s *Server) displayTimezone(w http.ResponseWriter, r *http.Request) {
	teamSetting := strings.HasPrefix(r.URL.Path, "/api/teams/")
	p := actor(r)
	team := p.Team
	if teamSetting {
		team = r.PathValue("id")
		if !s.teamAdmin(r, team) {
			http.Error(w, "permission denied", 403)
			return
		}
	}
	if r.Method == "PUT" {
		var in struct {
			Timezone string `json:"timezone"`
		}
		if !decode(w, r, &in) {
			return
		}
		if !validTimezone(in.Timezone, !teamSetting) {
			http.Error(w, "choose an IANA timezone such as Europe/London or UTC", 400)
			return
		}
		var err error
		if teamSetting {
			_, err = s.DB.Exec(r.Context(), `UPDATE teams SET display_timezone=$2 WHERE id=$1`, team, in.Timezone)
		} else {
			_, err = s.DB.Exec(r.Context(), `UPDATE users SET display_timezone=NULLIF($2,'') WHERE id=$1`, p.ID, in.Timezone)
		}
		if err != nil {
			http.Error(w, "could not save timezone", 503)
			return
		}
	}
	var inherited, override string
	if err := s.DB.QueryRow(r.Context(), `SELECT t.display_timezone,COALESCE(u.display_timezone,'') FROM teams t JOIN users u ON u.id=$1 WHERE t.id=$2`, p.ID, team).Scan(&inherited, &override); err != nil {
		http.Error(w, "timezone unavailable", 503)
		return
	}
	effective := inherited
	if override != "" {
		effective = override
	}
	respond(w, 200, map[string]string{"team_timezone": inherited, "user_timezone": override, "timezone": effective, "schedule_timezone": "UTC"})
}
