package main

import (
	"slices"
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
