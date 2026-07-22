package main

import (
	"slices"
	"strings"
	"testing"
)

// chunkQualifiers packs owner qualifiers into as few Search API queries as fit
// the length cap; a packing bug either silently drops owners (repos never
// watched) or overflows GitHub's 256-char query limit (search errors every
// cycle).
func TestChunkQualifiers(t *testing.T) {
	long := "org:" + strings.Repeat("a", maxQualifierChars-4) // exactly at the cap
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"Empty", nil, nil},
		{"Single", []string{"user:lao"}, []string{"user:lao"}},
		{"PacksIntoOne", []string{"user:lao", "org:acme"}, []string{"user:lao org:acme"}},
		{"SplitsAtCap", []string{long, "user:lao"}, []string{long, "user:lao"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkQualifiers(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Errorf("chunkQualifiers(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}

	t.Run("NoChunkExceedsCap", func(t *testing.T) {
		var owners []string
		for _, o := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"} {
			owners = append(owners, "org:"+strings.Repeat(o, 8))
		}
		for _, chunk := range chunkQualifiers(owners) {
			if len(chunk) > maxQualifierChars {
				t.Errorf("chunk %q is %d chars, cap %d", chunk, len(chunk), maxQualifierChars)
			}
		}
	})
}
