package worker

import "testing"

func TestWatchPaths(t *testing.T) {
	for _, tc := range []struct {
		patterns []string
		file     string
		want     bool
	}{
		{[]string{"**/*.go"}, "app.go", true},
		{[]string{"src/*"}, "src/nested/app.go", false},
		{[]string{"src/**"}, "src/nested/app.go", true},
		{[]string{"**", "!docs/**", "docs/keep.md"}, "docs/keep.md", true},
		{[]string{"**", "!docs/**"}, "docs/no.md", false},
		{[]string{"file?.go"}, "file1.go", true},
		{[]string{"[literal].go"}, "[literal].go", true},
	} {
		if got := WatchPathsMatch(tc.patterns, []string{tc.file}); got != tc.want {
			t.Errorf("%v %s: got %v", tc.patterns, tc.file, got)
		}
	}
	for _, p := range []string{"", "!", "/etc", "../secret", "a/../b", "a\\b"} {
		if ValidateWatchPaths([]string{p}) == nil {
			t.Errorf("accepted %q", p)
		}
	}
}
