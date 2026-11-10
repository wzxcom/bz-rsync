package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManualSyncFlow(t *testing.T) {
	r := testReceiver(t)
	if err := os.WriteFile(filepath.Join(r.stateDir, "authorized_keys"), []byte("paired"), 0600); err != nil {
		t.Fatal(err)
	}
	request := func(day string) int {
		req := httptest.NewRequest("POST", "/api/manual-sync", strings.NewReader(`{"day":"`+day+`"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.requestManual(w, req)
		return w.Code
	}
	yesterday := time.Now().In(beijing).AddDate(0, 0, -1).Format(time.DateOnly)
	if request(yesterday) != 200 || r.manual.Status != "queued" {
		t.Fatal("manual request was not queued")
	}
	id := r.manual.ID
	if request(yesterday) != 200 || r.manual.ID != id {
		t.Fatal("duplicate request replaced the queued task")
	}
	if request("2025-01-01") != 409 {
		t.Fatal("second request was allowed")
	}
	r, err := openReceiver(r.stateDir, r.backupDir)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := r.execute("bz-manual-claim", command{})
	if err != nil || claim.Manual.ID != id || claim.Manual.Status != "running" {
		t.Fatalf("claim did not resume persisted request: %+v %v", claim, err)
	}
	good := true
	if _, err := r.execute("bz-manual-result", command{RequestID: id, ConfigOK: &good}); err != nil || r.manual.Status != "success" {
		t.Fatalf("success was not recorded: %v", err)
	}
	claim, err = r.execute("bz-manual-claim", command{})
	if err != nil || claim.Manual != nil {
		t.Fatal("completed request was returned again")
	}
}
