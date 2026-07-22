package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	gogithub "github.com/google/go-github/v88/github"
	"github.com/lao/botbooter"
	"github.com/lao/botbooter/github"
)

// maxQualifierChars caps one query's owner qualifiers, leaving room for the
// fixed "is:pr created:>=..." prefix inside GitHub's 256-char query limit.
const maxQualifierChars = 200

// watchScope is where the watcher looks: qualifier strings that scope Search
// API queries ("repo:owner/name", or chunked "user:a org:b ..."), and the
// exact repositories the credentials can reach — search qualifiers over-match
// (user:X covers all of X's repos, granted or not), so hits are filtered
// against allowed before replying.
type watchScope struct {
	qualifiers []string
	allowed    map[string]bool // keyed "owner/name"
}

// resolveScope returns the explicit GITHUB_REPO scope when set, and otherwise
// discovers every repository the credentials can reach: the installation's
// granted repos in App mode, the token user's accessible repos in PAT mode.
// The wildcard "owner/*" (matching the adapter's ReactionPollRepos form — "*"
// is only special as the whole name) runs the same discovery narrowed to that
// owner, so it still only covers repos the credentials reach. Archived
// repositories are skipped — they cannot receive new PRs. Owners dedupe into
// user:/org: search qualifiers, chunked under the query length cap.
func resolveScope(ctx context.Context, bot *botbooter.Bot) (watchScope, error) {
	ownerFilter := ""
	if repo := os.Getenv("GITHUB_REPO"); repo != "" {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" {
			return watchScope{}, fmt.Errorf(`GITHUB_REPO must be "owner/name" or "owner/*", got %q`, repo)
		}
		if name != "*" {
			return watchScope{
				qualifiers: []string{"repo:" + repo},
				allowed:    map[string]bool{repo: true},
			}, nil
		}
		ownerFilter = owner
	}

	client := github.Client(bot)
	appMode := os.Getenv("GITHUB_PRIVATE_KEY_FILE") != ""
	scope := watchScope{allowed: map[string]bool{}}
	var owners []string // insertion-ordered for stable qualifiers
	seenOwner := map[string]bool{}
	page := 1
	for {
		var repos []*gogithub.Repository
		var resp *gogithub.Response
		var err error
		if appMode {
			var lst *gogithub.ListRepositories
			lst, resp, err = client.Apps.ListRepos(ctx, &gogithub.ListOptions{PerPage: 100, Page: page})
			if lst != nil {
				repos = lst.Repositories
			}
		} else {
			repos, resp, err = client.Repositories.ListByAuthenticatedUser(ctx,
				&gogithub.RepositoryListByAuthenticatedUserOptions{ListOptions: gogithub.ListOptions{PerPage: 100, Page: page}})
		}
		if err != nil {
			return watchScope{}, fmt.Errorf("discover repositories: %w", err)
		}
		for _, r := range repos {
			if r.GetArchived() {
				continue
			}
			// GitHub owner names are case-insensitive; match the wildcard the
			// same way so a casing mismatch doesn't silently watch nothing.
			if ownerFilter != "" && !strings.EqualFold(r.GetOwner().GetLogin(), ownerFilter) {
				continue
			}
			scope.allowed[r.GetFullName()] = true
			log.Printf("discovered %s", r.GetFullName())
			qualifier := "user:" + r.GetOwner().GetLogin()
			if r.GetOwner().GetType() == "Organization" {
				qualifier = "org:" + r.GetOwner().GetLogin()
			}
			if !seenOwner[qualifier] {
				seenOwner[qualifier] = true
				owners = append(owners, qualifier)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}
	scope.qualifiers = chunkQualifiers(owners)
	return scope, nil
}

// chunkQualifiers space-joins owner qualifiers into as few search queries as
// fit the query length cap; qualifiers of one kind OR together in GitHub
// search, so each chunk is a single query.
func chunkQualifiers(owners []string) []string {
	var chunks []string
	current := ""
	for _, o := range owners {
		switch {
		case current == "":
			current = o
		case len(current)+1+len(o) > maxQualifierChars:
			chunks = append(chunks, current)
			current = o
		default:
			current += " " + o
		}
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	return chunks
}
