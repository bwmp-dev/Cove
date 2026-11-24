package worker

import (
	"errors"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
)

type ImageUpdateRule struct {
	Service string `json:"service"`
	Range   string `json:"range"`
}

type ImageUpdatePolicy struct {
	Enabled     bool              `json:"enabled"`
	Mode        string            `json:"mode"`
	Rules       []ImageUpdateRule `json:"rules"`
	WindowStart string            `json:"window_start"`
	WindowEnd   string            `json:"window_end"`
}

func (p ImageUpdatePolicy) Validate() error {
	if p.Mode != "notify" && p.Mode != "auto" {
		return errors.New("choose notify or auto for image updates")
	}
	if len(p.Rules) == 0 || len(p.Rules) > 32 {
		return errors.New("choose between one and 32 image services")
	}
	seen := map[string]bool{}
	for _, rule := range p.Rules {
		if !serviceIdentifier.MatchString(rule.Service) || seen[rule.Service] {
			return errors.New("image update services must be valid and unique")
		}
		seen[rule.Service] = true
		if len(rule.Range) > 200 {
			return errors.New("image version ranges cannot exceed 200 characters")
		}
		if rule.Range != "" {
			if _, err := semver.NewConstraint(rule.Range); err != nil {
				return errors.New("invalid semantic version range")
			}
		}
	}
	return nil
}

// Equal-precedence tags are ambiguous image identities, even when only their v prefix or build metadata differs.
func selectImageUpdateTag(current, constraint string, tags []string) (string, error) {
	version, err := semver.StrictNewVersion(strings.TrimPrefix(current, "v"))
	if err != nil {
		return "", errors.New("the deployed image tag is not a complete semantic version")
	}
	rangeRule, err := semver.NewConstraint(constraint)
	if err != nil {
		return "", errors.New("invalid semantic version range")
	}
	type candidate struct {
		tag     string
		version *semver.Version
	}
	var choices []candidate
	seen := map[string]bool{}
	for _, tag := range tags {
		if seen[tag] {
			continue
		}
		seen[tag] = true
		v, parseErr := semver.StrictNewVersion(strings.TrimPrefix(tag, "v"))
		if parseErr == nil && rangeRule.Check(v) && v.GreaterThan(version) {
			choices = append(choices, candidate{tag, v})
		}
	}
	if len(choices) == 0 {
		return "", nil
	}
	sort.Slice(choices, func(i, j int) bool { return choices[i].version.GreaterThan(choices[j].version) })
	if len(choices) > 1 && choices[0].version.Equal(choices[1].version) {
		return "", errors.New("multiple tags identify the highest semantic version; select an unambiguous range")
	}
	return choices[0].tag, nil
}
