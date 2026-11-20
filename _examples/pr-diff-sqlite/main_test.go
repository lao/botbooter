package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v88/github"
)

func TestArchiveUpdatesDiffAndFallsBackToFilePatches(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/pulls":
			fmt.Fprint(w, `[{"number":1,"title":"PR","created_at":"2025-01-02T00:00:00Z"},{"number":2,"created_at":"2024-12-31T00:00:00Z"}]`)
		case "/repos/o/r/pulls/1":
			http.Error(w, "diff too large", http.StatusNotAcceptable)
		case "/repos/o/r/pulls/1/files":
			fmt.Fprint(w, `[{"filename":"new","previous_filename":"old","patch":"+line"},{"filename":"binary","status":"added","additions":0,"deletions":0}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := gogithub.NewClient(gogithub.WithURLs(&server.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := t.Context()
	if _, err = db.ExecContext(ctx, schema); err != nil {
		t.Fatal(err)
	}
	prs := newPullRequests(ctx, client, "o", "r", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	if len(prs) != 1 || prs[0].GetNumber() != 1 {
		t.Fatalf("new PRs = %v", prs)
	}
	if err := archive(ctx, client, db, "o", "r", prs[0]); err != nil {
		t.Fatal(err)
	}
	var diff string
	if err := db.QueryRow("SELECT diff FROM pr_diffs WHERE repo='o/r' AND number=1").Scan(&diff); err != nil {
		t.Fatal(err)
	}
	if want := "diff --git a/old b/new\n+line\ndiff --git a/binary b/binary\n(no inline patch: added, +0 -0)\n"; diff != want {
		t.Fatalf("stored diff = %q", diff)
	}

	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "updated diff") }))
	defer raw.Close()
	client, err = gogithub.NewClient(gogithub.WithURLs(&raw.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := archive(ctx, client, db, "o", "r", prs[0]); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.QueryRow("SELECT count(*), max(diff) FROM pr_diffs").Scan(&rows, &diff); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || diff != "updated diff" {
		t.Fatalf("rows = %d, diff = %q", rows, diff)
	}
}

func TestWatcherRetriesAndArchivesOnce(t *testing.T) {
	var polls, fetches atomic.Int32
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/pulls":
			if polls.Add(1) == 4 {
				cancel()
				return
			}
			created := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
			fmt.Fprintf(w, `[{"number":1,"created_at":%q},{"number":2,"draft":true,"created_at":%q}]`, created, created)
		case "/repos/o/r/pulls/1":
			if fetches.Add(1) == 1 {
				http.Error(w, "temporary error", 500)
				return
			}
			fmt.Fprint(w, "raw diff")
		case "/repos/o/r/pulls/1/files":
			http.Error(w, "temporary error", 500)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := gogithub.NewClient(gogithub.WithURLs(&server.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "watch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	watch(ctx, client, db, "o", "r", 100*time.Millisecond)
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatal("watcher did not complete four polls")
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("raw diff fetches = %d, want one retry", got)
	}
	var diff string
	if err = db.QueryRow("SELECT diff FROM pr_diffs WHERE repo='o/r' AND number=1").Scan(&diff); err != nil || diff != "raw diff" {
		t.Fatalf("archived diff = %q, %v", diff, err)
	}
}
