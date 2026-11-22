package panelurl

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var basePathPattern = regexp.MustCompile(`^(/[A-Za-z0-9_-]+)*$`)

func Parse(address string) (*url.URL, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || len(u.Path) > 200 || !basePathPattern.MatchString(u.Path) || strings.ContainsAny(address, "'\"\\?# \t\r\n\x00") {
		return nil, errors.New("use an HTTPS panel URL with an optional path of letters, numbers, underscores and hyphens, without a trailing slash")
	}
	if u.Path == "/healthz" {
		return nil, errors.New("/healthz is reserved for the panel liveness check; choose another base path")
	}
	return u, nil
}
