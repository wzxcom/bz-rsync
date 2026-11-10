package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTelegramManualMenuAndReplay(t *testing.T) {
	r := testReceiver(t)
	if err := os.WriteFile(filepath.Join(r.stateDir, "authorized_keys"), []byte("paired"), 0600); err != nil {
		t.Fatal(err)
	}
	settings := telegramSettings{Enabled: true, ChatID: "42", Token: fixtureBotToken, ManualSync: true}
	r.telegram = settings
	client := newTelegramClient()
	sendCount, pollCount := 0, 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Transport = telegramTransportFunc(func(req *http.Request) (*http.Response, error) {
		method := filepath.Base(req.URL.Path)
		data, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		answer := `{"ok":true,"result":true}`
		switch method {
		case "getWebhookInfo":
			answer = `{"ok":true,"result":{"url":""}}`
		case "setMyCommands":
			if !strings.Contains(string(data), `"scope":{"chat_id":42,"type":"chat"}`) ||
				!strings.Contains(string(data), `"command":"manual_sync"`) {
				t.Fatalf("menu is not registered to the intended chat: %s", data)
			}
		case "setChatMenuButton":
			if string(data) != `{"chat_id":42,"menu_button":{"type":"commands"}}` {
				t.Fatalf("native Telegram menu was not selected: %s", data)
			}
		case "sendMessage":
			sendCount++
			if sendCount == 1 && (!strings.Contains(string(data), `"text":"手动同步"`) ||
				!strings.Contains(string(data), `"is_persistent":true`)) {
				t.Fatalf("first message did not install the persistent button: %s", data)
			}
			if sendCount == 2 && !strings.Contains(string(data), "手动同步请求已提交") {
				t.Fatalf("manual request was not acknowledged: %s", data)
			}
		case "getUpdates":
			pollCount++
			var input struct {
				Offset int64 `json:"offset"`
			}
			if err := json.Unmarshal(data, &input); err != nil {
				t.Fatal(err)
			}
			if pollCount == 1 {
				if input.Offset != 0 {
					t.Fatalf("unexpected initial cursor: %d", input.Offset)
				}
				now := time.Now().Unix()
				answer = fmt.Sprintf(`{"ok":true,"result":[
					{"update_id":1,"message":{"chat":{"id":42,"type":"private"},"from":{"id":77},"date":%d,"text":"/manual_sync"}},
					{"update_id":2,"message":{"chat":{"id":42,"type":"private"},"from":{"id":42},"date":%d,"text":"/manual_sync"}},
					{"update_id":3,"message":{"chat":{"id":42,"type":"private"},"from":{"id":42},"date":%d,"text":"手动同步"}}
				]}`, now, now-200, now)
			} else {
				if input.Offset != 4 {
					t.Fatalf("manual action was not acknowledged with the correct cursor: %d", input.Offset)
				}
				cancel()
				answer = `{"ok":true,"result":[]}`
			}
		default:
			t.Fatalf("unexpected Telegram API call: %s", method)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(answer))}, nil
	})
	if err := r.receiveTelegramCommands(ctx, client, r.currentTelegramCommandConfig(), 42); err != nil {
		t.Fatal(err)
	}
	if sendCount != 2 || r.manual == nil || r.manual.Status != "queued" ||
		r.manual.Day != time.Now().In(beijing).Format(time.DateOnly) {
		t.Fatalf("expected one current-day queued request and two messages, got %+v and %d", r.manual, sendCount)
	}
	id := r.manual.ID
	if _, err := r.claimManual(); err != nil {
		t.Fatal(err)
	}
	success := true
	if _, err := r.reportManual(command{RequestID: id, ConfigOK: &success}); err != nil {
		t.Fatal(err)
	}
	r, err := openReceiver(r.stateDir, r.backupDir)
	if err != nil {
		t.Fatal(err)
	}
	r.telegram = settings
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	if err := r.receiveTelegramCommands(ctx, client, r.currentTelegramCommandConfig(), 42); err != nil {
		t.Fatal(err)
	}
	if r.manual.ID != id || r.manual.Status != "success" || sendCount != 2 {
		t.Fatal("restart re-queued a command or re-sent the menu message")
	}
}

func TestTelegramMenuDoesNotReplaceWebhook(t *testing.T) {
	client := newTelegramClient()
	client.Transport = telegramTransportFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/getWebhookInfo") {
			t.Fatal("menu setup proceeded despite existing webhook")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
			`{"ok":true,"result":{"url":"https://example.test/webhook"}}`))}, nil
	})
	if err := registerTelegramMenu(context.Background(), client, fixtureBotToken, 42); err != (telegramAPIError{code: http.StatusConflict}) {
		t.Fatalf("unexpected webhook result: %v", err)
	}
}
