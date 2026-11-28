package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistryTagsBasicPaginationAndCredentialScope(t *testing.T) {
	var calls int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		u, p, ok := r.BasicAuth()
		if !ok || u != "fixture" || p != "fixture-only" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(401)
			return
		}
		if r.URL.Query().Get("last") == "" {
			w.Header().Set("Link", `</v2/team/app/tags/list?n=256&last=1.0.0>; rel="next"`)
			fmt.Fprint(w, `{"name":"team/app","tags":["1.0.0"]}`)
		} else {
			fmt.Fprint(w, `{"name":"team/app","tags":["1.1.0"]}`)
		}
	}))
	defer s.Close()
	host := strings.TrimPrefix(s.URL, "http://")
	c := newRegistryTagClient()
	tags, err := c.tags(context.Background(), host+"/team/app", &Registry{Server: host, Username: "fixture", Password: "fixture-only"})
	if err != nil || len(tags) != 2 || calls != 4 {
		t.Fatalf("tags=%v calls=%d err=%v", tags, calls, err)
	}
	_, err = c.tags(context.Background(), host+"/team/app", &Registry{Server: "other.test", Username: "fixture", Password: "fixture-only"})
	if err == nil {
		t.Fatal("foreign credentials accepted")
	}
}

func TestRegistryTagsBearerAndBoundaries(t *testing.T) {
	var tokenCalls int
	var registryURL string
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenCalls++
			u, p, _ := r.BasicAuth()
			if u != "fixture" || p != "fixture-only" || r.URL.Query().Get("scope") != "repository:team/app:pull" {
				t.Error("incorrect token credentials or scope")
			}
			fmt.Fprint(w, `{"access_token":"fixture-access"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-access" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+registryURL+`/token",service="fixture",scope="repository:other:push"`)
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"name":"team/app","tags":["1.2.0"]}`)
	}))
	defer registry.Close()
	registryURL = registry.URL
	c := newRegistryTagClient()
	host := strings.TrimPrefix(registry.URL, "http://")
	tags, err := c.tags(context.Background(), host+"/team/app", &Registry{Server: host, Username: "fixture", Password: "fixture-only"})
	if err != nil || len(tags) != 1 || tokenCalls != 1 {
		t.Fatalf("tags=%v err=%v", tags, err)
	}
	for _, challenge := range []string{`Bearer realm="http://example.test/token"`, `Bearer realm="http://127.0.0.1:1/token"`, `Bearer realm="https://user:secret@example.test/token"`} {
		origin := registry.URL + "/v2/team/app/tags/list"
		request, _ := http.NewRequest("GET", origin, nil)
		if _, err = c.token(context.Background(), request.URL, "team/app", challenge, nil); err == nil {
			t.Fatal("unsafe token realm accepted")
		}
	}
}

func TestRegistryTagListingLimitsAndRedirects(t *testing.T) {
	for _, mode := range []string{"foreign page", "repeated page", "oversized", "too many tags", "redirect", "mismatched"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch mode {
				case "foreign page":
					w.Header().Set("Link", `<https://example.test/v2/team/app/tags/list>; rel="next"`)
				case "repeated page":
					w.Header().Set("Link", `</v2/team/app/tags/list?n=256>; rel="next"`)
				case "oversized":
					fmt.Fprint(w, strings.Repeat(" ", registryResponseLimit+1))
					return
				case "too many tags":
					fmt.Fprint(w, `{"name":"team/app","tags":[`+strings.Repeat(`"1.0.0",`, registryTagLimit)+`"2.0.0"]}`)
					return
				case "redirect":
					http.Redirect(w, r, "https://example.test/", 302)
					return
				case "mismatched":
					fmt.Fprint(w, `{"name":"other/app","tags":[]}`)
					return
				}
				fmt.Fprint(w, `{"name":"team/app","tags":[]}`)
			}))
			defer s.Close()
			if _, err := newRegistryTagClient().tags(context.Background(), strings.TrimPrefix(s.URL, "http://")+"/team/app", nil); err == nil {
				t.Fatal("unsafe listing accepted")
			}
			if calls > 2 {
				t.Fatalf("unexpected request count %d", calls)
			}
		})
	}
}
