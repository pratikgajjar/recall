package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionDotUsesPiIdentityNotNewestInCWD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	t.Setenv("RECALL_INDEX", path)
	t.Setenv("PI_SESSION_ID", "mine")
	ix, err := openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	sessions := []Session{
		{Source: "pi", SourceID: "mine", Project: project, StartedAt: 1000},
		{Source: "pi", SourceID: "other", Project: project, StartedAt: 2000},
	}
	msgs := []Message{
		{SourceID: "mine", Role: "user", Text: "distinctive apples"},
		{SourceID: "other", Role: "user", Text: "distinctive oranges"},
	}
	if err := ix.IngestBatch(context.Background(), "pi", sessions, msgs); err != nil {
		t.Fatal(err)
	}
	ix.Close()

	if err := runTag([]string{".", "deploy-rca"}); err != nil {
		t.Fatal(err)
	}
	ix, err = openIndexRead(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	mine, err := ix.SessionTags("pi:mine")
	if err != nil {
		t.Fatal(err)
	}
	other, err := ix.SessionTags("pi:other")
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0] != "deploy-rca" || len(other) != 0 {
		t.Fatalf("tag . selected wrong session: mine=%v other=%v", mine, other)
	}
	id, err := resolveSessionID(ix, ".")
	if err != nil || id != "pi:mine" {
		t.Fatalf("resolve . = %q, %v", id, err)
	}
	hits, err := ix.Search("distinctive", SearchOpts{SessionID: id})
	if err != nil || len(hits) != 1 || hits[0].SessionID != "pi:mine" {
		t.Fatalf("--in . scoped to wrong session: hits=%+v err=%v", hits, err)
	}
	if err := listTags([]string{"."}, false); err != nil {
		t.Fatal(err)
	}
	if err := runTag([]string{"-d", ".", "deploy-rca"}); err != nil {
		t.Fatal(err)
	}
	mine, err = ix.SessionTags("pi:mine")
	if err != nil || len(mine) != 0 {
		t.Fatalf("tag -d . did not remove tag from current session: %v, %v", mine, err)
	}
	if explicit, err := resolveSessionID(ix, "pi:other"); err != nil || explicit != "pi:other" {
		t.Fatalf("explicit id changed: %q, %v", explicit, err)
	}
}

func TestSessionDotFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	t.Setenv("RECALL_INDEX", path)
	ix, err := openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.IngestBatch(context.Background(), "pi", []Session{{Source: "pi", SourceID: "other"}}, []Message{{SourceID: "other", Role: "user", Text: "hello"}}); err != nil {
		t.Fatal(err)
	}
	ix.Close()

	t.Setenv("PI_SESSION_ID", "")
	if err := runTag([]string{".", "wrong-target"}); err == nil || !strings.Contains(err.Error(), "PI_SESSION_ID") {
		t.Fatalf("missing identity must fail clearly, got %v", err)
	}
	t.Setenv("PI_SESSION_ID", "not-indexed")
	if err := runTag([]string{".", "wrong-target"}); err == nil || !strings.Contains(err.Error(), "recall index") {
		t.Fatalf("unindexed identity must fail with index instruction, got %v", err)
	}
	if err := runFind([]string{"hello", "--in", "."}); err == nil || !strings.Contains(err.Error(), "recall index") {
		t.Fatalf("find --in . must reject unindexed identity, got %v", err)
	}
	ix, err = openIndexRead(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	tags, err := ix.SessionTags("pi:other")
	if err != nil || len(tags) != 0 {
		t.Fatalf("another session was tagged: %v, %v", tags, err)
	}
	if _, err := resolveSessionID(ix, "pi:other"); err != nil {
		t.Fatalf("explicit ID should work outside Pi: %v", err)
	}
}
