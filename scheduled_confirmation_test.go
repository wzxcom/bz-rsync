package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScheduledVerificationRecordsLatestAndNotifiesOnce(t *testing.T) {
	r := testReceiver(t)
	r.telegram = telegramSettings{Enabled: true, ChatID: "42", Token: fixtureBotToken}
	const day = "2026-11-08"
	digest := fixture(t, filepath.Join(r.backupDir, day))
	cmd := command{Day: day, TotalBytes: 150, Manifest: digest}
	check := func(r *receiver, cmd command) {
		t.Helper()
		if response, err := r.start(cmd); err != nil || !response.AlreadyDone {
			t.Fatalf("expected verified existing batch: %+v %v", response, err)
		}
	}
	check(r, cmd)
	cmd.Scheduled = true
	cmd.RequestID = strings.Repeat("a", 32)
	check(r, cmd)
	if r.scheduled != nil || len(r.telegramQueue) != 0 {
		t.Fatal("ordinary or manual verification recorded a scheduled confirmation")
	}
	cmd.RequestID = ""
	bad := cmd
	bad.Manifest = strings.Repeat("0", 64)
	if _, err := r.start(bad); err == nil || r.scheduled != nil || len(r.telegramQueue) != 0 {
		t.Fatal("verification of different content recorded a confirmation")
	}
	before := time.Now()
	check(r, cmd)
	record := r.scheduled
	if record == nil || record.Day != day || record.TotalBytes != 150 ||
		record.CheckedAt.Before(before) || record.CheckedAt.After(time.Now()) {
		t.Fatalf("incorrect scheduled confirmation: %+v", record)
	}
	if len(r.telegramQueue) != 1 {
		t.Fatal("first scheduled confirmation should enqueue one notification")
	}
	notice := <-r.telegramQueue
	text := r.formatTelegramNotice(notice)
	if notice.day != day || !strings.Contains(text, "定时复核通过，批次已入库") ||
		!strings.Contains(text, record.CheckedAt.Format("2006-01-02 15:04:05")) ||
		!strings.Contains(text, "批次大小</b>  0.00 GB") || strings.Contains(text, "任务耗时") ||
		!strings.Contains(text, "网站归档大小</b>  0.00 GB") || !strings.Contains(text, "数据库归档大小</b>  0.00 GB") {
		t.Fatal("scheduled confirmation was not distinguished from a new transfer")
	}
	if len(r.runs) != 0 {
		t.Fatal("verification added a transfer history row")
	}
	if _, err := os.Stat(filepath.Join(r.stateDir, "history.json")); !os.IsNotExist(err) {
		t.Fatalf("verification wrote transfer history: %v", err)
	}
	public := httptest.NewRecorder()
	r.state(public, httptest.NewRequest("GET", "/api/state", nil))
	var response struct {
		Scheduled *scheduledConfirmation `json:"scheduled_confirmation"`
	}
	if err := json.Unmarshal(public.Body.Bytes(), &response); err != nil || response.Scheduled == nil ||
		response.Scheduled.Day != day || !response.Scheduled.CheckedAt.Equal(record.CheckedAt) ||
		response.Scheduled.TotalBytes != record.TotalBytes {
		t.Fatalf("incorrect public state: %+v, %v", response, err)
	}
	reopened, err := openReceiver(r.stateDir, r.backupDir)
	if err != nil {
		t.Fatal(err)
	}
	reopened.telegram = r.telegram
	check(reopened, cmd)
	older := cmd
	older.Day = "2026-11-07"
	older.Manifest = fixture(t, filepath.Join(r.backupDir, older.Day))
	check(reopened, older)
	if len(reopened.telegramQueue) != 0 || reopened.scheduled.Day != day ||
		!reopened.scheduled.CheckedAt.Equal(record.CheckedAt) {
		t.Fatal("same-day or older retry altered the confirmation after restart")
	}
	newer := cmd
	newer.Day = "2026-11-09"
	newer.Manifest = fixture(t, filepath.Join(r.backupDir, newer.Day))
	check(reopened, newer)
	if len(reopened.telegramQueue) != 1 || reopened.scheduled.Day != newer.Day {
		t.Fatal("newer confirmed date did not replace the latest confirmation")
	}
}

func TestScheduledConfirmationWithTelegramDisabledAndAfterFreshTransfer(t *testing.T) {
	r := testReceiver(t)
	const existing = "2026-11-07"
	digest := fixture(t, filepath.Join(r.backupDir, existing))
	cmd := command{Day: existing, TotalBytes: 150, Manifest: digest, Scheduled: true}
	if response, err := r.start(cmd); err != nil || !response.AlreadyDone ||
		r.scheduled == nil || r.scheduled.Day != existing || len(r.telegramQueue) != 0 {
		t.Fatalf("disabled Telegram prevented durable confirmation or enqueued a notice: %+v %v", response, err)
	}
	r.telegram = telegramSettings{Enabled: true, ChatID: "42", Token: fixtureBotToken}
	const fresh = "2026-11-08"
	cmd.Day = fresh
	start, err := r.start(cmd)
	if err != nil || start.AlreadyDone {
		t.Fatalf("new scheduled transfer start: %+v %v", start, err)
	}
	fixture(t, r.staging(fresh))
	for _, action := range []string{"claim", "release", "bz-finish"} {
		if _, err := r.execute(action, command{RunID: start.RunID}); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.telegramQueue) != 1 || (<-r.telegramQueue).kind != "同步成功" ||
		r.scheduled.Day != existing {
		t.Fatal("fresh scheduled transfer should report its normal result only")
	}
	reopened, err := openReceiver(r.stateDir, r.backupDir)
	if err != nil {
		t.Fatal(err)
	}
	reopened.telegram = r.telegram
	if response, err := reopened.start(cmd); err != nil || !response.AlreadyDone ||
		reopened.scheduled.Day != fresh || len(reopened.telegramQueue) != 0 {
		t.Fatalf("recheck after a fresh scheduled success sent a duplicate notice: %+v %v", response, err)
	}
}
