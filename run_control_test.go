package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPauseResumeThenCancelStage(t *testing.T) {
	r := testReceiver(t)
	day := "2026-11-07"
	digest := strings.Repeat("a", 64)
	payload := command{Day: day, Manifest: digest, TotalBytes: 128}
	start, err := r.start(payload)
	if err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(r.staging(day), "partial")
	if err := os.WriteFile(partial, []byte("received"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.execute("claim", command{RunID: start.RunID}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.applyRunAction(start.RunID, "pause"); err != nil {
		t.Fatal(err)
	}
	if polled, err := r.controlForRun(start.RunID); err != nil || polled.Action != "pause" {
		t.Fatalf("sender did not see pause: %+v %v", polled, err)
	}
	if _, err := r.execute("release", command{RunID: start.RunID, Error: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if r.runs[0].Status != "paused" {
		t.Fatal("not paused after transfer exited")
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("pause removed staged data: %v", err)
	}
	if _, err := r.start(payload); err == nil {
		t.Fatal("scheduled retry restarted paused data")
	}
	resume, err := r.applyRunAction(start.RunID, "resume")
	if err != nil || resume.Manual == nil || resume.Manual.Manifest != digest {
		t.Fatalf("resume was not pinned to original manifest: %+v %v", resume, err)
	}
	claim, err := r.claimManual()
	if err != nil || claim.Manual.ID != resume.Manual.ID {
		t.Fatalf("resume request was not claimable: %+v %v", claim, err)
	}
	payload.RequestID = claim.Manual.ID
	second, err := r.start(payload)
	if err != nil || second.RunID == start.RunID {
		t.Fatalf("resume did not start: %+v %v", second, err)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("resume lost staged data: %v", err)
	}
	if _, err := r.execute("claim", command{RunID: second.RunID}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.applyRunAction(second.RunID, "cancel"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.execute("claim", command{RunID: second.RunID}); err == nil {
		t.Fatal("a second claim could control a still-active transfer")
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("cancel removed files before receiver exited: %v", err)
	}
	other := filepath.Join(r.backupDir, "unrelated")
	if err := os.WriteFile(other, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.execute("release", command{RunID: second.RunID, Error: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if r.runs[0].Status != "cancelled" || r.manual.Status != "cancelled" {
		t.Fatal("cancelled states were not recorded")
	}
	if _, err := os.Stat(r.staging(day)); !os.IsNotExist(err) {
		t.Fatal("cancelled stage still exists", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("unrelated backup disappeared", err)
	}
}
func TestAdoptLegacyPartial(t *testing.T) {
	stage := t.TempDir()
	partial := filepath.Join(stage, "database", ".bz-rsync-partial")
	if err := os.MkdirAll(partial, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(partial, "backup.gz")
	if err := os.WriteFile(file, []byte("older bytes worth reusing"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := adoptLegacyPartials(stage); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(stage, "database", "backup.gz"))
	if err != nil || string(data) != "older bytes worth reusing" {
		t.Fatal("legacy partial was not adopted", err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatal("legacy directory was not removed", err)
	}
}
