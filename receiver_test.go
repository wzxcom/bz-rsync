package main

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, path string) string {
	t.Helper()
	for _, group := range []string{"site", "database"} {
		if err := os.MkdirAll(filepath.Join(path, group), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, group, "backup.gz"), []byte(group), 0640); err != nil {
			t.Fatal(err)
		}
	}
	data := fmt.Appendf(nil, "%x  site/backup.gz\n%x  database/backup.gz\n", sha256.Sum256([]byte("site")), sha256.Sum256([]byte("database")))
	if err := os.WriteFile(filepath.Join(path, "SHA256SUMS"), data, 0640); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func testReceiver(t *testing.T) *receiver {
	t.Helper()
	r, err := openReceiver(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestVerifiedPublicationAndCorruption(t *testing.T) {
	r := testReceiver(t)
	for _, day := range []string{"2026-01-02", "2026-01-03"} {
		digest := fixture(t, t.TempDir())
		start, err := r.start(command{Day: day, Manifest: digest, TotalBytes: 150})
		if err != nil {
			t.Fatal(err)
		}
		// Populate only after the receiver accepts the run.
		fixture(t, r.staging(day))
		for _, action := range []string{"claim", "release"} {
			if _, err := r.execute(action, command{RunID: start.RunID}); err != nil {
				t.Fatal(err)
			}
		}
		corrupt := day == "2026-01-03"
		if corrupt {
			err := os.WriteFile(filepath.Join(r.staging(day), "site", "backup.gz"), []byte("changed"), 0640)
			if err != nil {
				t.Fatal(err)
			}
		}
		_, err = r.finish(start.RunID)
		_, statErr := os.Stat(filepath.Join(r.backupDir, day))
		if corrupt {
			if err == nil || !os.IsNotExist(statErr) || r.retention.Managed[day] != "" {
				t.Fatalf("corrupt batch published or recorded: %v, %v", err, statErr)
			}
		} else {
			if err != nil || statErr != nil || r.retention.Managed[day] != digest {
				t.Fatalf("verified batch missing: %v, %v", err, statErr)
			}
			duplicate, err := r.start(command{Day: day, Manifest: digest, TotalBytes: 150})
			if err != nil || !duplicate.AlreadyDone {
				t.Fatalf("expected idempotent duplicate: %+v %v", duplicate, err)
			}
		}
	}
}

func TestRetentionPreservesBoundaryAndUnmanaged(t *testing.T) {
	r := testReceiver(t)
	now := time.Now().In(beijing)
	expired := now.AddDate(0, 0, -5).Format(time.DateOnly)
	boundary := now.AddDate(0, 0, -4).Format(time.DateOnly)
	unmanaged := now.AddDate(0, 0, -6).Format(time.DateOnly)
	changed := now.AddDate(0, 0, -7).Format(time.DateOnly)
	for _, day := range []string{expired, boundary, unmanaged, changed} {
		digest := fixture(t, filepath.Join(r.backupDir, day))
		if day != unmanaged {
			r.retention.Managed[day] = digest
		}
	}
	if err := os.WriteFile(filepath.Join(r.backupDir, changed, "unexpected"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	r.cleanupExpired()
	if _, err := os.Stat(filepath.Join(r.backupDir, expired)); err != nil {
		t.Fatal("default disabled must preserve expired batch", err)
	}
	r.retention.Days = 5
	r.cleanupExpired()
	if _, err := os.Stat(filepath.Join(r.backupDir, expired)); !os.IsNotExist(err) {
		t.Fatal("expired managed batch remains", err)
	}
	for _, day := range []string{boundary, unmanaged, changed} {
		if _, err := os.Stat(filepath.Join(r.backupDir, day)); err != nil {
			t.Fatal("protected batch was removed", day, err)
		}
	}
	if r.retention.LastCleanup == nil || r.retention.CleanupError == "" {
		t.Fatal("cleanup result must report skipped changed batch")
	}
}

func TestPasswordOnlyLoginAndChange(t *testing.T) {
	r := testReceiver(t)
	login, err := openAuth(r.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	h := r.web(login, "unused.html")
	request := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := request("GET", "/api/state", "", nil); rec.Code != 401 {
		t.Fatalf("expected unauthenticated 401, got %d", rec.Code)
	}
	old := request("POST", "/api/login", `{"password":"password"}`, nil)
	if old.Code != 200 || len(old.Result().Cookies()) != 1 {
		t.Fatalf("default login failed: %d %s", old.Code, old.Body.String())
	}
	oldCookie := old.Result().Cookies()[0]
	rec := request("POST", "/api/password", `{"current_password":"password","new_password":"test-new-pass"}`, oldCookie)
	if rec.Code != 200 || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("password change failed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request("GET", "/api/state", "", oldCookie); rec.Code != 401 {
		t.Fatal("old session remains valid")
	}
	if rec := request("POST", "/api/retention", `{"days":7}`, rec.Result().Cookies()[0]); rec.Code != 200 || r.retention.Days != 7 {
		t.Fatalf("retention settings update failed: %d", rec.Code)
	}
	reloaded, err := openAuth(r.stateDir)
	if err != nil || !reloaded.matches("test-new-pass") || reloaded.matches("password") {
		t.Fatal("password update did not survive reload")
	}
}
