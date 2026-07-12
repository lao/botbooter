// Command pr-diff-sqlite watches one repository for newly created pull
// requests and archives each PR's entire unified diff into a local SQLite
// database, building a corpus that can later be fed to an LLM (review
// generation, summarization, fine-tuning data, ...).
//
//	go run ./_examples/pr-diff-sqlite   # reads GITHUB_TOKEN, GITHUB_REPO (and optional GITHUB_PR_POLL_SECONDS, PR_DIFF_DB)
//
// Drafts are deliberately not archived: a draft is an unfinished diff, so the
// watcher waits until the PR is (created as, or marked) ready for review.
// Like the github-prs example, PR creation is observed by polling rather than
// webhook — the botbooter adapter's webhook ingress is issue_comment only, and
// polling needs no public URL. Each cycle (GITHUB_PR_POLL_SECONDS, default 30)
// lists the repo's open PRs newest-first and stops at the first one created
// before startup, so only PRs created while the watcher runs are archived; a
// PR opened as a draft after startup is picked up on the cycle where it flips
// to ready (a draft that predates startup is not).
//
// The diff is fetched whole via the raw-diff media type (GET /pulls/{n} with
// Accept: application/vnd.github.diff). GitHub refuses that endpoint for very
// large PRs, so on error the watcher falls back to stitching the per-file
// patches from the list-files endpoint; files there without a patch (binary,
// or individually too large) are recorded as a placeholder header so the LLM
// still sees the file existed. A failed fetch is retried next cycle.
//
// One row per PR, keyed (repo, number), upserted so a re-fetch (e.g. ready →
// draft → ready again) keeps the newest diff:
//
//	sqlite3 pr_diffs.db "SELECT repo, number, title, length(diff) FROM pr_diffs"
//	sqlite3 pr_diffs.db "SELECT diff FROM pr_diffs WHERE number = 42"
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	gogithub "github.com/google/go-github/v88/github"
	"github.com/joho/godotenv"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS pr_diffs (
	repo       TEXT    NOT NULL,
	number     INTEGER NOT NULL,
	title      TEXT    NOT NULL,
	author     TEXT    NOT NULL,
	base_ref   TEXT    NOT NULL,
	head_ref   TEXT    NOT NULL,
	head_sha   TEXT    NOT NULL,
	created_at TEXT    NOT NULL, -- PR creation time, RFC 3339 UTC
	fetched_at TEXT    NOT NULL, -- when the diff was archived, RFC 3339 UTC
	diff       TEXT    NOT NULL,
	PRIMARY KEY (repo, number)
)`

const upsert = `
INSERT INTO pr_diffs (repo, number, title, author, base_ref, head_ref, head_sha, created_at, fetched_at, diff)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (repo, number) DO UPDATE SET
	title = excluded.title, author = excluded.author,
	base_ref = excluded.base_ref, head_ref = excluded.head_ref, head_sha = excluded.head_sha,
	fetched_at = excluded.fetched_at, diff = excluded.diff`

func main() {
	_ = godotenv.Load(".env")

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		log.Fatal("GITHUB_TOKEN is required")
	}
	repo := os.Getenv("GITHUB_REPO")
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		log.Fatalf(`GITHUB_REPO must be "owner/name", got %q`, repo)
	}
	interval := 30 * time.Second
	if s := os.Getenv("GITHUB_PR_POLL_SECONDS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			log.Fatalf("parse GITHUB_PR_POLL_SECONDS %q", s)
		}
		interval = time.Duration(n) * time.Second
	}
	dbPath := os.Getenv("PR_DIFF_DB")
	if dbPath == "" {
		dbPath = "pr_diffs.db"
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("open %s: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(schema); err != nil {
		log.Fatalf("create table: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := gogithub.NewClient(gogithub.WithAuthToken(token))
	if err != nil {
		log.Fatalf("new github client: %v", err)
	}
	log.Printf("watching %s for new non-draft pull requests every %s, archiving diffs to %s", repo, interval, dbPath)
	watch(ctx, client, db, owner, name, interval)
}

// watch polls for open PRs created after startup and archives each non-draft
// one exactly once (unless its fetch fails, which retries next cycle).
func watch(ctx context.Context, client *gogithub.Client, db *sql.DB, owner, name string, every time.Duration) {
	since := time.Now()
	archived := map[int]bool{}

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		for _, pr := range newPullRequests(ctx, client, owner, name, since) {
			if pr.GetDraft() || archived[pr.GetNumber()] {
				continue
			}
			if err := archive(ctx, client, db, owner, name, pr); err != nil {
				log.Printf("archive %s/%s#%d: %v", owner, name, pr.GetNumber(), err)
				continue
			}
			archived[pr.GetNumber()] = true
			log.Printf("archived %s/%s#%d %q by %s", owner, name, pr.GetNumber(), pr.GetTitle(), pr.GetUser().GetLogin())
		}
	}
}

// newPullRequests lists the repo's open PRs newest-first and returns the ones
// created at or after since, paging only as far as that cutoff.
func newPullRequests(ctx context.Context, client *gogithub.Client, owner, name string, since time.Time) []*gogithub.PullRequest {
	var out []*gogithub.PullRequest
	opt := &gogithub.PullRequestListOptions{
		State: "open", Sort: "created", Direction: "desc",
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}
	for {
		prs, resp, err := client.PullRequests.List(ctx, owner, name, opt)
		if err != nil {
			log.Printf("list pull requests: %v", err)
			return out
		}
		for _, pr := range prs {
			if pr.GetCreatedAt().Before(since) {
				return out
			}
			out = append(out, pr)
		}
		if resp.NextPage == 0 {
			return out
		}
		opt.Page = resp.NextPage
	}
}

// archive fetches the PR's entire diff and upserts it with the PR metadata.
func archive(ctx context.Context, client *gogithub.Client, db *sql.DB, owner, name string, pr *gogithub.PullRequest) error {
	num := pr.GetNumber()
	diff, _, err := client.PullRequests.GetRaw(ctx, owner, name, num, gogithub.RawOptions{Type: gogithub.Diff})
	if err != nil {
		// The raw-diff endpoint refuses very large PRs; stitch per-file patches instead.
		log.Printf("raw diff for #%d failed (%v), falling back to per-file patches", num, err)
		if diff, err = stitchedDiff(ctx, client, owner, name, num); err != nil {
			return err
		}
	}

	_, err = db.ExecContext(ctx, upsert,
		owner+"/"+name, num, pr.GetTitle(), pr.GetUser().GetLogin(),
		pr.GetBase().GetRef(), pr.GetHead().GetRef(), pr.GetHead().GetSHA(),
		pr.GetCreatedAt().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339),
		diff)
	return err
}

// stitchedDiff rebuilds a unified diff from the list-files endpoint, one
// "diff --git" section per file. Files without a patch (binary, or too large
// for the API to inline) keep their header so the reader knows they changed.
func stitchedDiff(ctx context.Context, client *gogithub.Client, owner, name string, num int) (string, error) {
	var b strings.Builder
	opt := &gogithub.ListOptions{PerPage: 100}
	for {
		files, resp, err := client.PullRequests.ListFiles(ctx, owner, name, num, opt)
		if err != nil {
			return "", fmt.Errorf("list files: %w", err)
		}
		for _, f := range files {
			from := f.GetPreviousFilename()
			if from == "" {
				from = f.GetFilename()
			}
			fmt.Fprintf(&b, "diff --git a/%s b/%s\n", from, f.GetFilename())
			if p := f.GetPatch(); p != "" {
				b.WriteString(p)
				b.WriteString("\n")
			} else {
				fmt.Fprintf(&b, "(no inline patch: %s, +%d -%d)\n", f.GetStatus(), f.GetAdditions(), f.GetDeletions())
			}
		}
		if resp.NextPage == 0 {
			return b.String(), nil
		}
		opt.Page = resp.NextPage
	}
}
