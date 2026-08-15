package search

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlaceholderPathRoundTrip(t *testing.T) {
	doc := filepath.Join("/vault", "Financial", "Invoice_Alex-Rao_Acme_2026.pdf")
	ph := PlaceholderPath(doc)
	if filepath.Base(ph) != ".Invoice_Alex-Rao_Acme_2026.pdf.icloud" {
		t.Fatalf("placeholder name: %s", ph)
	}
	if got := documentForPlaceholder(ph); got != doc {
		t.Errorf("round trip: got %s want %s", got, doc)
	}
}

func TestMaterializedStates(t *testing.T) {
	dir := t.TempDir()

	full := filepath.Join(dir, "full.txt")
	if err := os.WriteFile(full, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	evicted := filepath.Join(dir, "evicted.txt")
	if err := os.WriteFile(PlaceholderPath(evicted), []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file that exists but still has a placeholder beside it: iCloud has not
	// finished, and its bytes must not be trusted.
	partial := filepath.Join(dir, "partial.txt")
	if err := os.WriteFile(partial, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PlaceholderPath(partial), []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name             string
		path             string
		materialized     bool
		evictedPredicate bool
	}{
		{"non-empty regular file", full, true, false},
		{"empty file", empty, false, false},
		{"placeholder only", evicted, false, true},
		{"file with a placeholder beside it", partial, false, true},
		{"missing entirely", filepath.Join(dir, "nope.txt"), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Materialized(tc.path); got != tc.materialized {
				t.Errorf("Materialized = %v, want %v", got, tc.materialized)
			}
			if got := IsEvicted(tc.path); got != tc.evictedPredicate {
				t.Errorf("IsEvicted = %v, want %v", got, tc.evictedPredicate)
			}
		})
	}
}

// TestWaitMaterializedTimeout is the safety invariant behind Materialize: the
// wait fails rather than reporting success on a file whose bytes are not there.
// It needs no brctl and no iCloud.
func TestWaitMaterializedTimeout(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name  string
		setup func(path string)
		want  string
	}{
		{
			name:  "still a placeholder",
			setup: func(p string) { writeFile(t, PlaceholderPath(p), "stub") },
			want:  "still an iCloud placeholder",
		},
		{
			name:  "never appeared",
			setup: func(string) {},
			want:  "file does not exist",
		},
		{
			name:  "appeared but empty",
			setup: func(p string) { writeFile(t, p, "") },
			want:  "file is empty",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "doc"+string(rune('a'+i))+".txt")
			tc.setup(path)
			err := waitMaterialized(context.Background(), path, 30*time.Millisecond, 5*time.Millisecond)
			if err == nil {
				t.Fatal("want a timeout error, got success")
			}
			if !errors.Is(err, ErrNotMaterialized) {
				t.Fatalf("want ErrNotMaterialized, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not explain %q", err, tc.want)
			}
		})
	}
}

func TestWaitMaterializedSucceedsWhenTheFileArrives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "arriving.txt")
	writeFile(t, PlaceholderPath(path), "stub")

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(20 * time.Millisecond)
		writeFile(t, path, "the real bytes")
		os.Remove(PlaceholderPath(path))
	}()

	if err := waitMaterialized(context.Background(), path, 5*time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("want success once the file arrives: %v", err)
	}
	<-done
}

func TestWaitMaterializedHonoursContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "never.txt")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitMaterialized(ctx, path, time.Minute, 5*time.Millisecond)
	if !errors.Is(err, ErrNotMaterialized) {
		t.Fatalf("want ErrNotMaterialized, got %v", err)
	}
}

func TestMaterializeIsANoOpForAPresentFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "present.txt")
	writeFile(t, path, "bytes")
	if err := Materialize(context.Background(), path); err != nil {
		t.Fatalf("a file already on disk must not need brctl: %v", err)
	}
}

func TestMaterializeFailsForAMissingFile(t *testing.T) {
	err := Materialize(context.Background(), filepath.Join(t.TempDir(), "nope.txt"))
	if err == nil {
		t.Fatal("want an error for a file that is neither present nor evicted")
	}
	if errors.Is(err, ErrNoBrctl) {
		t.Fatal("a missing file should not be reported as a missing brctl")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
