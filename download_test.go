package main

import (
	"archive/zip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSenderDownloadRequiresLogin(t *testing.T) {
	r := testReceiver(t)
	login, err := openAuth(r.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "sender.zip")
	file, err := os.Create(bundle)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	if _, err := archive.Create("sender/install.sh"); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BZ_SENDER_DOWNLOAD", bundle)
	web := r.web(login, "unused.html")
	request := func(method, target, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		reply := httptest.NewRecorder()
		web.ServeHTTP(reply, req)
		return reply
	}
	if got := request("GET", "/downloads/sender.zip", "", nil); got.Code != 401 {
		t.Fatalf("anonymous download status %d", got.Code)
	}
	cookies := request("POST", "/api/login", `{"password":"password"}`, nil).Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("could not log in")
	}
	reply := request("GET", "/downloads/sender.zip", "", cookies[0])
	if reply.Code != 200 || reply.Header().Get("Content-Type") != "application/zip" ||
		!strings.Contains(reply.Header().Get("Content-Disposition"), "bz-rsync-sender-"+releaseVersion()+".zip") ||
		!strings.HasPrefix(reply.Body.String(), "PK") {
		t.Fatalf("download did not return a versioned archive: %d %v", reply.Code, reply.Header())
	}
}
