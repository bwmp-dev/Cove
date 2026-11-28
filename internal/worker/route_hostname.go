package worker

import (
	"regexp"
	"strings"
)

func RouteDomainsOverlap(a, b string) bool {
	return a == b || routeDomainMatches(a, b) || routeDomainMatches(b, a)
}

func routeDomainMatches(pattern, hostname string) bool {
	if !strings.HasPrefix(pattern, "*.") || strings.HasPrefix(hostname, "*.") || !strings.HasSuffix(hostname, pattern[1:]) {
		return false
	}
	label := strings.TrimSuffix(hostname, pattern[1:])
	return domainLabel.MatchString(label)
}

func routeHostRule(domain string) string {
	if strings.HasPrefix(domain, "*.") {
		return "HostRegexp(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\\." + regexp.QuoteMeta(domain[2:]) + "$`)"
	}
	return "Host(`" + domain + "`)"
}

func routePriority(route Route) int {
	if strings.HasPrefix(route.Domain, "*.") {
		// A negative range keeps every exact host ahead without changing its existing rule-derived priority.
		return -2048 + len(route.pathPrefix())
	}
	return 0
}

func routeProbeHostname(domain string, domains map[string]bool) string {
	if !strings.HasPrefix(domain, "*.") {
		return domain
	}
	// Thirty-six candidates exceed the per-resource route limit, even at the maximum DNS length.
	for _, label := range "abcdefghijklmnopqrstuvwxyz0123456789" {
		host := string(label) + domain[1:]
		if !domains[host] {
			return host
		}
	}
	return ""
}
