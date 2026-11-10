package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func claimReviewTestRequest(t *testing.T, r *receiver, day string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.stateDir, "authorized_keys"), []byte("paired"), 0600); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	code, submitted := r.submitManualLocked(day)
	r.mu.Unlock()
	if code != 200 || !submitted.OK || submitted.Manual == nil {
		t.Fatalf("could not submit manual request: %d %+v", code, submitted)
	}
	claimed, err := r.claimManual()
	if err != nil || claimed.Manual == nil || claimed.Manual.ID != submitted.Manual.ID || claimed.Manual.Status != "running" {
		t.Fatalf("could not claim manual request: %+v %v", claimed, err)
	}
	return claimed.Manual.ID
}

func publishReviewTestBatch(t *testing.T, r *receiver, day string) (string, archiveBreakdown) {
	t.Helper()
	stage := r.staging(day)
	digest := fixture(t, stage)
	manifest, err := os.Stat(filepath.Join(stage, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	sizes := archiveBreakdown{
		SiteBytes: int64(len("site")), DatabaseBytes: int64(len("database")),
		TotalBytes: int64(len("site")+len("database")) + manifest.Size(),
	}
	if err := r.publish(day, digest); err != nil {
		t.Fatal(err)
	}
	r.removePublishMarker(day)
	return digest, sizes
}

func TestManualReviewPersistsUntilSenderReportsSuccess(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("split=%v", split), func(t *testing.T) {
			r, err := openReceiverLayout(t.TempDir(), t.TempDir(), split)
			if err != nil {
				t.Fatal(err)
			}
			r.telegram = telegramSettings{Enabled: true, ChatID: "42", Token: fixtureBotToken}
			day := time.Now().In(beijing).Format(time.DateOnly)
			digest, expected := publishReviewTestBatch(t, r, day)
			id := claimReviewTestRequest(t, r, day)
			started := time.Now()
			// Use a sender total that differs from the actual published archive.
			start, err := r.start(command{Day: day, Manifest: digest, TotalBytes: 999, RequestID: id})
			if err != nil || !start.AlreadyDone || start.RunID != "" || len(r.runs) != 0 ||
				r.manual.Status != "running" || len(r.telegramQueue) != 0 {
				t.Fatalf("recheck changed the deployed sender handshake or notified before its report: %+v %v", start, err)
			}
			review := r.manual.Review
			if review == nil || review.Sizes != expected || review.Elapsed < 0 || review.Elapsed > time.Since(started) {
				t.Fatalf("incorrect verified review metadata: %+v, want sizes %+v", review, expected)
			}

			reopened, err := openReceiverLayout(r.stateDir, r.backupDir, split)
			if err != nil {
				t.Fatal(err)
			}
			reopened.telegram = r.telegram
			if reopened.manual == nil || reopened.manual.Review == nil || reopened.manual.Review.Sizes != review.Sizes ||
				reopened.manual.Review.Elapsed != review.Elapsed || reopened.manual.Status != "running" ||
				len(reopened.telegramQueue) != 0 {
				t.Fatalf("manual review did not persist as a running request: %+v", reopened.manual)
			}
			good := true
			report := command{RequestID: id, ConfigOK: &good}
			result, err := reopened.reportManual(report)
			if err != nil || !result.OK || reopened.manual.Status != "success" || len(reopened.telegramQueue) != 1 {
				t.Fatalf("legacy sender success report did not finalize one recheck: %+v %v", result, err)
			}
			notice := <-reopened.telegramQueue
			text := reopened.formatTelegramNotice(notice)
			if notice.kind != "手动复核" {
				t.Fatalf("wrong notification kind: %s", notice.kind)
			}
			for _, expectedText := range []string{"手动复核成功", day, "批次大小", "网站归档大小", "数据库归档大小", "复核耗时"} {
				if !strings.Contains(text, expectedText) {
					t.Fatalf("manual review notice omitted %q", expectedText)
				}
			}
			result, err = reopened.reportManual(report)
			if err != nil || !result.OK || len(reopened.telegramQueue) != 0 {
				t.Fatalf("duplicate sender success report did not remain idempotent: %+v %v", result, err)
			}
		})
	}
}

func TestManualReviewRetryAfterFreshPublishDoesNotNotifyTwice(t *testing.T) {
	r := testReceiver(t)
	r.telegram = telegramSettings{Enabled: true, ChatID: "42", Token: fixtureBotToken}
	day := time.Now().In(beijing).Format(time.DateOnly)
	id := claimReviewTestRequest(t, r, day)
	digest := fixture(t, t.TempDir())
	input := command{Day: day, Manifest: digest, TotalBytes: 150, RequestID: id}
	start, err := r.start(input)
	if err != nil || start.AlreadyDone || start.RunID == "" {
		t.Fatalf("fresh transfer did not start: %+v %v", start, err)
	}
	fixture(t, r.staging(day))
	for _, action := range []string{"claim", "release", "bz-finish"} {
		if _, err := r.execute(action, command{RunID: start.RunID}); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.telegramQueue) != 1 || (<-r.telegramQueue).kind != "同步成功" {
		t.Fatal("fresh transfer should send its normal success notification")
	}

	// The sender retries the same still-running manual request after losing its reply.
	r, err = openReceiver(r.stateDir, r.backupDir)
	if err != nil {
		t.Fatal(err)
	}
	r.telegram = telegramSettings{Enabled: true, ChatID: "42", Token: fixtureBotToken}
	claimed, err := r.claimManual()
	if err != nil || claimed.Manual == nil || claimed.Manual.ID != id {
		t.Fatalf("could not reclaim the original request: %+v %v", claimed, err)
	}
	again, err := r.start(input)
	if err != nil || !again.AlreadyDone || len(r.telegramQueue) != 0 {
		t.Fatalf("retry did not take the no-transfer branch: %+v %v", again, err)
	}
	good := true
	result, err := r.reportManual(command{RequestID: id, ConfigOK: &good})
	if err != nil || !result.OK || r.manual.Status != "success" || len(r.telegramQueue) != 0 {
		t.Fatalf("report after fresh-publish retry duplicated a notification: %+v %v", result, err)
	}
}

func TestManualReviewDoesNotRecordWrongRequest(t *testing.T) {
	r := testReceiver(t)
	r.telegram = telegramSettings{Enabled: true, ChatID: "42", Token: fixtureBotToken}
	day := time.Now().In(beijing).Format(time.DateOnly)
	digest, _ := publishReviewTestBatch(t, r, day)
	id := claimReviewTestRequest(t, r, day)
	start, err := r.start(command{Day: day, Manifest: digest, TotalBytes: 150, Scheduled: true, RequestID: "other-request"})
	if err != nil || !start.AlreadyDone || r.manual.ID != id || r.manual.Review != nil ||
		r.scheduled != nil || len(r.telegramQueue) != 0 {
		t.Fatalf("synthetic mismatched request changed review state: %+v %v", start, err)
	}
	_, err = r.start(command{Day: day, Manifest: strings.Repeat("0", 64), TotalBytes: 150, RequestID: id})
	if err == nil || r.manual.Review != nil || len(r.telegramQueue) != 0 {
		t.Fatal("mismatched archive digest produced a manual review")
	}
}

func TestManualPreparationFailureNotificationIsSanitized(t *testing.T) {
	r := testReceiver(t)
	r.telegram = telegramSettings{Enabled: true, ChatID: "42", Token: fixtureBotToken}
	day := time.Now().In(beijing).Format(time.DateOnly)
	id := claimReviewTestRequest(t, r, day)
	failed := false
	const privateDetail = "private internal sender error"
	report := command{RequestID: id, ConfigOK: &failed, Error: privateDetail + " " + fixtureBotToken}
	result, err := r.reportManual(report)
	if err != nil || !result.OK || r.manual.Status != "failed" || len(r.runs) != 0 || len(r.telegramQueue) != 1 {
		t.Fatalf("preparation failure did not produce one notice: %+v %v", result, err)
	}
	text := r.formatTelegramNotice(<-r.telegramQueue)
	if !strings.Contains(text, "手动同步失败") || !strings.Contains(text, day) ||
		strings.Contains(text, privateDetail) || strings.Contains(text, fixtureBotToken) || strings.Contains(text, "归档大小") {
		t.Fatal("preparation failure notice omitted safe context or leaked unverified details")
	}
	result, err = r.reportManual(report)
	if err != nil || !result.OK || len(r.telegramQueue) != 0 {
		t.Fatalf("duplicate sender failure report did not remain idempotent: %+v %v", result, err)
	}
}
