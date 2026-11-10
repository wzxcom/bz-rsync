package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const fixtureBotToken = "123456:fixture_token-not_a_real_secret"

type telegramTransportFunc func(*http.Request) (*http.Response, error)

func (fn telegramTransportFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestTelegramSettingsAndSavedTestMessage(t *testing.T) {
	r := testReceiver(t)
	login := &auth{sessions: map[string]time.Time{"valid": time.Now().Add(time.Minute)}}
	web := r.web(login, "unused.html")
	request := func(method, path, body string, signedIn bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if signedIn {
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "valid"})
		}
		response := httptest.NewRecorder()
		web.ServeHTTP(response, req)
		if strings.Contains(response.Body.String(), fixtureBotToken) {
			t.Fatal("private token returned by HTTP API")
		}
		return response
	}
	if got := request("GET", "/api/telegram", "", false); got.Code != 401 {
		t.Fatalf("unsigned request status = %d", got.Code)
	}
	if got := request("GET", "/api/telegram", "", true); got.Code != 200 ||
		got.Body.String() != "{\"enabled\":false,\"chat_id\":\"\",\"has_token\":false}\n" {
		t.Fatalf("unexpected initial settings: %d %s", got.Code, got.Body)
	}
	if got := request("POST", "/api/telegram", `{"enabled":true,"chat_id":"42","token":""}`, true); got.Code != 400 {
		t.Fatalf("enabled without token: %d", got.Code)
	}
	if got := request("POST", "/api/telegram", `{"enabled":false,"chat_id":"0","token":""}`, true); got.Code != 400 {
		t.Fatalf("invalid chat ID accepted: %d", got.Code)
	}
	initial := `{"enabled":true,"chat_id":"42","token":"` + fixtureBotToken + `"}`
	if got := request("POST", "/api/telegram", initial, true); got.Code != 200 {
		t.Fatalf("save failed: %d %s", got.Code, got.Body)
	}
	if got := request("POST", "/api/telegram", `{"enabled":false,"chat_id":"-100420","token":""}`, true); got.Code != 200 ||
		got.Body.String() != "{\"enabled\":false,\"chat_id\":\"-100420\",\"has_token\":true}\n" {
		t.Fatalf("unexpected replacement settings: %d %s", got.Code, got.Body)
	}
	info, err := os.Stat(filepath.Join(r.stateDir, telegramStateFile))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("state file permissions = %o", info.Mode().Perm())
	}
	reopened, err := openReceiver(r.stateDir, r.backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.telegram != r.telegram || r.telegram.Token != fixtureBotToken {
		t.Fatal("blank token did not preserve saved credentials across restart")
	}
	calls := 0
	r.telegramClient.Transport = telegramTransportFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodPost || req.URL.Scheme != "https" || req.URL.Host != "api.telegram.org" ||
			req.URL.Path != "/bot"+fixtureBotToken+"/sendMessage" {
			t.Error("unexpected Telegram destination or method")
		}
		var data struct {
			ChatID    int64  `json:"chat_id"`
			Text      string `json:"text"`
			ParseMode string `json:"parse_mode"`
		}
		if err := json.NewDecoder(req.Body).Decode(&data); err != nil || data.ChatID != -100420 ||
			data.ParseMode != "HTML" || !strings.Contains(data.Text, "<b>🧪 bz-rsync · 测试通知</b>") ||
			!strings.Contains(data.Text, "示例批次大小</b>  12.66 GB") ||
			!strings.Contains(data.Text, "示例网站归档大小</b>  8.60 GB") ||
			!strings.Contains(data.Text, "示例数据库归档大小</b>  4.06 GB") || strings.Contains(data.Text, fixtureBotToken) {
			t.Error("invalid test message or saved destination")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
	})
	if got := request("POST", "/api/telegram/test", "{}", true); got.Code != 200 || calls != 1 {
		t.Fatalf("disabled saved test failed: %d, calls %d", got.Code, calls)
	}
}

func TestTelegramTemplateEscapesHTMLAndFormatsDecimalSize(t *testing.T) {
	message := telegramMessage{
		title: "标题 <标签>", day: "日期 & 内容", sizeLabel: "大小",
		totalBytes: 12_661_617_936, timeLabel: "耗时", timeValue: telegramDuration(4*time.Minute + 18*time.Second),
		detail: "详情 <b> & 文本",
	}
	text := message.format()
	for _, fragment := range []string{
		"<b>标题 &lt;标签&gt;</b>", "日期 &amp; 内容", "12.66 GB", "4 分 18 秒", "详情 &lt;b&gt; &amp; 文本",
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("missing template fragment %q", fragment)
		}
	}
}

func TestTelegramDeliveryRedactsErrorsAndRejectsRedirects(t *testing.T) {
	client := newTelegramClient()
	settings := telegramSettings{ChatID: "@test_channel", Token: fixtureBotToken}
	calls := 0
	client.Transport = telegramTransportFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Header:     http.Header{"Location": {"https://untrusted.example/"}},
			Body:       io.NopCloser(strings.NewReader(fixtureBotToken)),
		}, nil
	})
	if err := sendTelegram(context.Background(), client, settings, "text"); err == nil ||
		calls != 1 || strings.Contains(err.Error(), fixtureBotToken) {
		t.Fatal("redirect followed or credential leaked")
	}
	client.Transport = telegramTransportFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New(req.URL.String())
	})
	if err := sendTelegram(context.Background(), client, settings, "text"); err == nil ||
		strings.Contains(err.Error(), fixtureBotToken) {
		t.Fatal("transport error was returned without credential redaction")
	}
}

func TestTelegramNoticeOncePerFinalizedRun(t *testing.T) {
	r := testReceiver(t)
	r.telegram = telegramSettings{Enabled: true, ChatID: "42", Token: fixtureBotToken}
	digest := fixture(t, t.TempDir())
	start := func(day string) string {
		t.Helper()
		run, err := r.start(command{Day: day, TotalBytes: 150, Manifest: digest})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.telegramQueue) != 0 {
			t.Fatal("running transfer produced a notification")
		}
		return run.RunID
	}
	day := "2026-11-08"
	id := start(day)
	fixture(t, r.staging(day))
	for _, action := range []string{"claim", "release", "bz-finish"} {
		if _, err := r.execute(action, command{RunID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.telegramQueue) != 1 {
		t.Fatal("successful published batch did not enqueue one notice")
	}
	notice := <-r.telegramQueue
	text := r.formatTelegramNotice(notice)
	if !strings.Contains(text, "同步成功") || !strings.Contains(text, day) ||
		!strings.Contains(text, "批次大小</b>  0.00 GB") || !strings.Contains(text, "任务耗时") ||
		!strings.Contains(text, "网站归档大小</b>  0.00 GB") || !strings.Contains(text, "数据库归档大小</b>  0.00 GB") {
		t.Fatal("success notice omitted batch details")
	}
	if repeat, err := r.start(command{Day: day, TotalBytes: 150, Manifest: digest}); err != nil ||
		!repeat.AlreadyDone || len(r.telegramQueue) != 0 {
		t.Fatal("idempotent retry generated a duplicate notice")
	}
	id = start("2026-11-09")
	if _, err := r.execute("bz-fail", command{RunID: id, Error: fixtureBotToken}); err != nil {
		t.Fatal(err)
	}
	if len(r.telegramQueue) != 1 {
		t.Fatal("failed run did not enqueue one notice")
	}
	notice = <-r.telegramQueue
	text = r.formatTelegramNotice(notice)
	if !strings.Contains(text, "发送端传输失败") || !strings.Contains(text, "计划批次大小") ||
		strings.Contains(text, "归档大小") || strings.Contains(text, fixtureBotToken) {
		t.Fatal("failure notice forwarded unsanitized text")
	}
	_, _ = r.execute("bz-fail", command{RunID: id, Error: "duplicate"})
	if len(r.telegramQueue) != 0 {
		t.Fatal("duplicate terminal command generated a duplicate notice")
	}
}
