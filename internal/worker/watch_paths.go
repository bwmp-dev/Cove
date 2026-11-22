package worker

import (
	"errors"
	"regexp"
	"strings"
)

func ValidateWatchPaths(patterns []string) error {
	if len(patterns) > 64 {
		return errors.New("at most 64 watch paths are allowed")
	}
	for _, pattern := range patterns {
		p := strings.TrimPrefix(pattern, "!")
		if p == "" || len(pattern) > 256 || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00\r\n\t") {
			return errors.New("invalid watch path")
		}
		for _, part := range strings.Split(p, "/") {
			if part == "." || part == ".." {
				return errors.New("watch paths must be repository-relative")
			}
		}
	}
	return nil
}

func WatchPathsMatch(patterns, files []string) bool {
	if len(patterns) == 0 {
		return true
	}
	type rule struct {
		expression *regexp.Regexp
		exclude    bool
	}
	rules := make([]rule, 0, len(patterns))
	for _, pattern := range patterns {
		exclude := strings.HasPrefix(pattern, "!")
		chars := []rune(strings.TrimPrefix(pattern, "!"))
		var expression strings.Builder
		expression.WriteString("^")
		for i := 0; i < len(chars); i++ {
			switch chars[i] {
			case '*':
				if i+1 < len(chars) && chars[i+1] == '*' {
					i++
					if i+1 < len(chars) && chars[i+1] == '/' {
						i++
						expression.WriteString("(?:.*/)?")
					} else {
						expression.WriteString(".*")
					}
				} else {
					expression.WriteString("[^/]*")
				}
			case '?':
				expression.WriteString("[^/]")
			default:
				expression.WriteString(regexp.QuoteMeta(string(chars[i])))
			}
		}
		expression.WriteString("$")
		rules = append(rules, rule{regexp.MustCompile(expression.String()), exclude})
	}
	for _, file := range files {
		matched := false
		for _, rule := range rules {
			if rule.expression.MatchString(file) {
				matched = !rule.exclude
			}
		}
		if matched {
			return true
		}
	}
	return false
}
