package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// splitRepos feeds Config.ReactionPollRepos, whose library validation accepts
// a leading-space owner as part of the name — so trimming here is what stands
// between "lao/botbooter, lao/other" and silently polling the wrong repo.
func TestSplitRepos(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"Single", "lao/botbooter", []string{"lao/botbooter"}},
		{"List", "lao/botbooter,lao/other", []string{"lao/botbooter", "lao/other"}},
		{"TrimsAndDropsEmpties", " lao/botbooter , lao/other ,", []string{"lao/botbooter", "lao/other"}},
		{"AllEmptyBehavesLikeUnset", ", ,", nil},
		{"Unset", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitRepos(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Errorf("splitRepos(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// autoIntervalDisabled feeds Config.ReactionPollNoAutoInterval: unset keeps
// the adapter's automatic budget-driven interval raise, "off" disables it,
// and anything unrecognized must fail startup rather than silently keep the
// default.
func TestAutoIntervalDisabled(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    bool
		wantErr bool
	}{
		{"Unset", "", false, false},
		{"On", "on", false, false},
		{"True", "true", false, false},
		{"One", "1", false, false},
		{"Off", "off", true, false},
		{"FalseUppercase", "FALSE", true, false},
		{"Zero", "0", true, false},
		{"Garbage", "banana", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := autoIntervalDisabled(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("autoIntervalDisabled(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("autoIntervalDisabled(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestGitHubPollingConfigCheckedBeforeAppKey(t *testing.T) {
	t.Setenv("GITHUB_REPO", "owner/repo")
	t.Setenv("GITHUB_POLL_SECONDS", "invalid")
	t.Setenv("GITHUB_PRIVATE_KEY_FILE", filepath.Join(t.TempDir(), "missing"))
	_, err := newGitHubBot()
	if err == nil || !strings.Contains(err.Error(), "GITHUB_POLL_SECONDS") {
		t.Fatalf("newGitHubBot error = %v", err)
	}
}
