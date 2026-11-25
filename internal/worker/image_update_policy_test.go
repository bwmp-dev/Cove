package worker

import "testing"

func TestImageUpdateVersionSelection(t *testing.T) {
	for _, tc := range []struct {
		name, current, rule string
		tags                []string
		want                string
		fail                bool
	}{
		{"numeric", "1.2.3", "^1.2.0", []string{"1.2.9", "1.2.10", "2.0.0"}, "1.2.10", false},
		{"minor", "v1.2.0", "~1.2", []string{"v1.2.1", "1.3.0"}, "v1.2.1", false},
		{"stable", "1.0.0", ">=1.0.0 <2", []string{"1.1.0-beta.1", "1.0.1"}, "1.0.1", false},
		{"prerelease", "1.1.0-beta.1", ">=1.1.0-beta.1 <1.1.0", []string{"1.1.0-beta.2"}, "1.1.0-beta.2", false},
		{"no downgrade", "2.0.0", "^1", []string{"1.9.0"}, "", false},
		{"ambiguous", "1.0.0", "^1", []string{"1.1.0", "v1.1.0"}, "", true},
		{"malformed", "latest", "*", []string{"1.0.0"}, "", true},
		{"invalid range", "1.0.0", "invalid", nil, "", true},
		{"ignore malformed", "1.0.0", "*", []string{"1.2", "01.2.3", "foo", "1.0.0"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectImageUpdateTag(tc.current, tc.rule, tc.tags)
			if (err != nil) != tc.fail || got != tc.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

func TestImageUpdatePolicyValidation(t *testing.T) {
	p := ImageUpdatePolicy{Mode: "notify", Rules: []ImageUpdateRule{{Service: "web", Range: "^1.2"}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.Rules = append(p.Rules, p.Rules[0])
	if p.Validate() == nil {
		t.Fatal("duplicate service accepted")
	}
}
