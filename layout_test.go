package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchivedBreakdownInBothLayouts(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("split=%v", split), func(t *testing.T) {
			r, err := openReceiverLayout(t.TempDir(), t.TempDir(), split)
			if err != nil {
				t.Fatal(err)
			}
			const day = "2026-11-08"
			stage := r.staging(day)
			fixture(t, stage)
			otherSite := []byte("another site")
			if err := os.WriteFile(filepath.Join(stage, "site", "second.gz"), otherSite, 0640); err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(stage, "SHA256SUMS")
			data, err := os.ReadFile(manifest)
			if err != nil {
				t.Fatal(err)
			}
			data = fmt.Appendf(data, "%x  site/second.gz\n", sha256.Sum256(otherSite))
			if err := os.WriteFile(manifest, data, 0640); err != nil {
				t.Fatal(err)
			}
			if err := r.publish(day, fmt.Sprintf("%x", sha256.Sum256(data))); err != nil {
				t.Fatal(err)
			}
			siteDir := filepath.Join(r.backupDir, day, "site")
			if split {
				siteDir = r.layoutDate("site", day)
				r.paths.Current = archivePaths{"new-site", "new-database"}
			}
			got, err := r.archivedBreakdown(day)
			want := archiveBreakdown{SiteBytes: int64(len("site") + len(otherSite)), DatabaseBytes: int64(len("database")),
				TotalBytes: int64(len("site") + len(otherSite) + len("database") + len(data))}
			if err != nil || got != want {
				t.Fatalf("incorrect breakdown: %+v, %v", got, err)
			}
			if err := os.Remove(filepath.Join(siteDir, "second.gz")); err != nil {
				t.Fatal(err)
			}
			if _, err := r.archivedBreakdown(day); err == nil {
				t.Fatal("missing manifest file did not make the breakdown unavailable")
			}
			message := telegramMessage{title: "成功", day: day, sizeLabel: "批次大小", totalBytes: 150}
			text := r.formatTelegramNotice(telegramNotice{kind: "同步成功", day: day, message: message})
			if !strings.Contains(text, "批次大小") || strings.Contains(text, "归档大小") {
				t.Fatal("unavailable breakdown suppressed the total or fabricated a category")
			}
		})
	}
}

func TestSplitLayoutPublicationAndRetention(t *testing.T) {
	state, root := t.TempDir(), t.TempDir()
	r, err := openReceiverLayout(state, root, true)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Now().In(beijing).AddDate(0, 0, -7).Format(time.DateOnly)
	digest := fixture(t, t.TempDir())
	payload := command{Day: day, Manifest: digest, TotalBytes: 150}
	start, err := r.start(payload)
	if err != nil {
		t.Fatal(err)
	}
	fixture(t, r.staging(day))
	for _, parent := range []string{".", "database"} {
		if err := os.Mkdir(filepath.Join(r.staging(day), parent, ".bz-rsync-partial"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, action := range []string{"claim", "release"} {
		if _, err := r.execute(action, command{RunID: start.RunID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.finish(start.RunID); err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"site", "database"} {
		if _, err := os.Stat(filepath.Join(r.layoutDate(group, day), "backup.gz")); err != nil {
			t.Fatal(err)
		}
	}
	duplicate, err := r.start(payload)
	if err != nil || !duplicate.AlreadyDone {
		t.Fatalf("split batch should be idempotent: %+v %v", duplicate, err)
	}
	_, content, err := manifestDigests(r.splitMeta(day))
	if err != nil {
		t.Fatal(err)
	}
	reordered := payload
	reordered.Manifest, reordered.Content = strings.Repeat("0", 64), content
	duplicate, err = r.start(reordered)
	if err != nil || !duplicate.AlreadyDone {
		t.Fatalf("same content in a different manifest order should be skipped: %+v %v", duplicate, err)
	}
	reordered.Content = strings.Repeat("1", 64)
	if _, err := r.start(reordered); err == nil {
		t.Fatal("different content must not be treated as a duplicate")
	}
	legacy := filepath.Join(root, day)
	fixture(t, legacy)
	unknownDay := time.Now().In(beijing).AddDate(0, 0, -8).Format(time.DateOnly)
	existing := r.layoutDate("site", unknownDay)
	if err := os.Mkdir(existing, 0750); err != nil {
		t.Fatal(err)
	}
	if _, err := r.start(command{Day: unknownDay, Manifest: digest, TotalBytes: 150}); err == nil {
		t.Fatal("pre-existing split folder must conflict")
	}
	r.retention.Days = 5
	r.cleanupExpired()
	for _, group := range []string{"site", "database"} {
		if _, err := os.Stat(r.layoutDate(group, day)); !os.IsNotExist(err) {
			t.Fatalf("expired split folder was not removed: %v", err)
		}
	}
	for _, keep := range []string{legacy, existing} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("unmanaged folder disappeared: %s %v", keep, err)
		}
	}
}

func TestPartialContentIsNotDiscarded(t *testing.T) {
	root := t.TempDir()
	partial := filepath.Join(root, ".bz-rsync-partial")
	if err := os.Mkdir(partial, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(partial, "backup.gz")
	if err := os.WriteFile(file, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeEmptyPartials(root); err == nil {
		t.Fatal("non-empty partial directory should block publication")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("unfinished file was discarded", err)
	}
}

func TestSplitPublishRecovery(t *testing.T) {
	state, root := t.TempDir(), t.TempDir()
	r, err := openReceiverLayout(state, root, true)
	if err != nil {
		t.Fatal(err)
	}
	day := "2026-11-07"
	digest := fixture(t, t.TempDir())
	start, err := r.start(command{Day: day, Manifest: digest, TotalBytes: 150})
	if err != nil {
		t.Fatal(err)
	}
	stage := r.staging(day)
	fixture(t, stage)
	r.runs[0].Status = "verifying"
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stage+".publish", []byte(digest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(stage, "site"), r.layoutDate("site", day)); err != nil {
		t.Fatal(err)
	}
	r, err = openReceiverLayout(state, root, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.runs[0].ID != start.RunID || r.runs[0].Status != "success" {
		t.Fatal("unfinished split publish did not recover")
	}
	if err := r.verifyPublished(day, digest); err != nil {
		t.Fatal(err)
	}
}
