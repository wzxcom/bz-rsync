package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigurePathsPreservesLegacyBatch(t *testing.T) {
	state, root := t.TempDir(), t.TempDir()
	r, err := openReceiverLayout(state, root, true)
	if err != nil {
		t.Fatal(err)
	}
	day := "2026-11-07"
	stage := r.staging(day)
	digest := fixture(t, stage)
	for _, category := range []string{"site", "database"} {
		if err := os.Rename(filepath.Join(stage, category), r.layoutDate(category, day)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(stage, r.splitMeta(day)); err != nil {
		t.Fatal(err)
	}
	r.retention.Managed[day] = digest
	if err := r.saveRetention(); err != nil {
		t.Fatal(err)
	}
	next := archivePaths{"web-archives", "db-archives"}
	if err := r.configurePaths(next); err != nil {
		t.Fatal(err)
	}
	r, err = openReceiverLayout(state, root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.verifyPublished(day, digest); err != nil {
		t.Fatalf("old batch became inaccessible after changing paths: %v", err)
	}
	if got := r.layoutDate("site", "2026-11-08"); got != filepath.Join(root, next.Site, "20261108") {
		t.Fatalf("unexpected next batch path: %s", got)
	}
	if _, err := r.start(command{Day: "2026-11-08", Manifest: digest, TotalBytes: 10}); err != nil {
		t.Fatal(err)
	}
	if err := r.configurePaths(archivePaths{"another-site", "another-db"}); err == nil {
		t.Fatal("path edit during transfer was allowed")
	}
}

func TestRejectUnsafeArchiveDirectoryNames(t *testing.T) {
	for _, names := range []archivePaths{
		{"../escape", "database"},
		{"site", "/absolute"},
		{"site", "SITE"},
		{".bz-rsync-staging", "database"},
		{"site/subfolder", "database"},
	} {
		if names.valid() {
			t.Fatalf("unsafe pair accepted: %+v", names)
		}
	}
}
