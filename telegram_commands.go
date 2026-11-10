package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	telegramControlFile   = "telegram-control.json"
	telegramManualLabel   = "手动同步"
	telegramManualCommand = "manual_sync"
)

type telegramReplyKeyboard struct {
	Keyboard [][]struct {
		Text string `json:"text"`
	} `json:"keyboard"`
	Persistent bool `json:"is_persistent"`
	Resize     bool `json:"resize_keyboard"`
}

var telegramManualKeyboard = &telegramReplyKeyboard{
	Keyboard: [][]struct {
		Text string `json:"text"`
	}{{{Text: telegramManualLabel}}},
	Persistent: true,
	Resize:     true,
}

func (settings telegramSettings) commandChatID() int64 {
	id, err := strconv.ParseInt(settings.ChatID, 10, 64)
	if err != nil || id <= 0 || !settings.Enabled || !settings.ManualSync || settings.Token == "" {
		return 0
	}
	return id
}

type telegramControlState struct {
	Identity     string    `json:"identity"`
	KeyboardSent bool      `json:"keyboard_sent"`
	UpdateID     int64     `json:"update_id"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (r *receiver) loadTelegramControl(settings telegramSettings, chatID int64) (telegramControlState, error) {
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(settings.Token+"\x00"+strconv.FormatInt(chatID, 10))))
	path := filepath.Join(r.stateDir, telegramControlFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return telegramControlState{Identity: identity}, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 2048 {
		return telegramControlState{}, errors.New("Telegram 手动同步状态文件无效")
	}
	data, err := os.ReadFile(path)
	var state telegramControlState
	if err != nil || json.Unmarshal(data, &state) != nil || state.Identity == "" || state.UpdateID < 0 {
		return telegramControlState{}, errors.New("无法读取 Telegram 手动同步状态")
	}
	if state.Identity != identity {
		return telegramControlState{Identity: identity}, nil
	}
	return state, nil
}

func (r *receiver) saveTelegramControl(state telegramControlState) error {
	data, err := json.Marshal(state)
	if err == nil {
		err = writeAtomic(r.stateDir, telegramControlFile, data)
	}
	if err != nil {
		return errors.New("无法保存 Telegram 手动同步状态")
	}
	return nil
}

func registerTelegramMenu(ctx context.Context, client *http.Client, token string, chatID int64) error {
	var webhook struct {
		URL string `json:"url"`
	}
	if err := callTelegram(ctx, client, token, "getWebhookInfo", struct{}{}, &webhook); err != nil {
		return err
	}
	if webhook.URL != "" {
		return telegramAPIError{code: http.StatusConflict}
	}
	if err := callTelegram(ctx, client, token, "setMyCommands", map[string]any{
		"scope":    map[string]any{"type": "chat", "chat_id": chatID},
		"commands": []map[string]string{{"command": telegramManualCommand, "description": telegramManualLabel}},
	}, nil); err != nil {
		return err
	}
	return callTelegram(ctx, client, token, "setChatMenuButton", map[string]any{
		"chat_id": chatID, "menu_button": map[string]string{"type": "commands"},
	}, nil)
}

type telegramUpdate struct {
	ID      int64                `json:"update_id"`
	Message telegramMessageInput `json:"message"`
}

type telegramMessageInput struct {
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	From struct {
		ID    int64 `json:"id"`
		IsBot bool  `json:"is_bot"`
	} `json:"from"`
	Date    int64           `json:"date"`
	Text    string          `json:"text"`
	Forward json.RawMessage `json:"forward_origin"`
}

func (message telegramMessageInput) command(chatID int64, now time.Time) string {
	if message.Chat.Type != "private" || message.Chat.ID != chatID || message.From.ID != chatID ||
		message.From.IsBot || len(message.Forward) != 0 {
		return ""
	}
	sent := time.Unix(message.Date, 0)
	if sent.Before(now.Add(-2*time.Minute)) || sent.After(now.Add(30*time.Second)) {
		return ""
	}
	switch strings.TrimSpace(message.Text) {
	case telegramManualLabel, "/" + telegramManualCommand:
		return telegramManualCommand
	case "/start", "/help":
		return "start"
	default:
		return ""
	}
}

type telegramCommandConfig struct {
	telegramSettings
	revision uint64
}

func (r *receiver) currentTelegramCommandConfig() telegramCommandConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	return telegramCommandConfig{r.telegram, r.telegramRevision}
}

func (r *receiver) requestTelegramManual(config telegramCommandConfig, now time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.telegram != config.telegramSettings || r.telegramRevision != config.revision || config.commandChatID() == 0 {
		return ""
	}
	day := now.In(beijing).Format(time.DateOnly)
	_, response := r.submitManualLocked(day)
	if !response.OK {
		return "⚠️ " + html.EscapeString(response.Error)
	}
	return "✅ <b>手动同步请求已提交</b>\n📅 批次日期 " + day + "（北京时间）\n宝塔发送端会在下次检查时领取任务。"
}

func telegramPause(ctx context.Context, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (r *receiver) telegramCommandWorker(ctx context.Context) {
	client := newTelegramClient()
	client.Timeout = 30 * time.Second
	for ctx.Err() == nil {
		config := r.currentTelegramCommandConfig()
		chatID := config.commandChatID()
		if chatID == 0 {
			telegramPause(ctx, 2*time.Second)
			continue
		}
		if err := r.receiveTelegramCommands(ctx, client, config, chatID); err != nil && ctx.Err() == nil {
			r.waitTelegramCommandRetry(ctx, config, err)
		}
	}
}

func (r *receiver) waitTelegramCommandRetry(ctx context.Context, config telegramCommandConfig, err error) {
	log.Printf("Telegram 手动同步菜单暂不可用：%s", err)
	delay := 15 * time.Second
	var apiError telegramAPIError
	if errors.As(err, &apiError) {
		if apiError.code == http.StatusConflict {
			delay = time.Minute
		}
		delay = max(delay, apiError.retryAfter)
	}
	deadline := time.Now().Add(delay)
	for ctx.Err() == nil && r.currentTelegramCommandConfig() == config {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		telegramPause(ctx, min(remaining, 2*time.Second))
	}
}

func (r *receiver) receiveTelegramCommands(ctx context.Context, client *http.Client, config telegramCommandConfig, chatID int64) error {
	activatedAt := time.Now().Truncate(time.Second)
	settings := config.telegramSettings
	state, err := r.loadTelegramControl(settings, chatID)
	if err != nil {
		return err
	}
	if err := registerTelegramMenu(ctx, client, settings.Token, chatID); err != nil {
		return err
	}
	const welcome = "点击「手动同步」提交北京时间当天的备份同步请求。"
	log.Print("Telegram 手动同步菜单已启用")

	var offset int64
	var nextKeyboardAttempt time.Time
	lastUpdate := state.UpdatedAt
	if state.UpdateID > 0 && time.Since(lastUpdate) < 24*time.Hour {
		offset = state.UpdateID + 1
	}
	for ctx.Err() == nil && r.currentTelegramCommandConfig() == config {
		if !state.KeyboardSent && time.Now().After(nextKeyboardAttempt) {
			if err := sendTelegramMessage(ctx, client, settings, welcome, telegramManualKeyboard); err != nil {
				log.Printf("Telegram 常驻按钮发送失败：%s", err)
				nextKeyboardAttempt = time.Now().Add(time.Minute)
			} else {
				state.KeyboardSent = true
				if err := r.saveTelegramControl(state); err != nil {
					return err
				}
			}
		}
		if time.Since(lastUpdate) >= 24*time.Hour {
			offset = 0
		}
		var updates []telegramUpdate
		if err := callTelegram(ctx, client, settings.Token, "getUpdates", map[string]any{
			"offset": offset, "limit": 10, "timeout": 20, "allowed_updates": []string{"message"},
		}, &updates); err != nil {
			if ctx.Err() == nil && r.currentTelegramCommandConfig() == config {
				r.waitTelegramCommandRetry(ctx, config, err)
			}
			continue
		}
		for _, update := range updates {
			if ctx.Err() != nil || r.currentTelegramCommandConfig() != config {
				return nil
			}
			if update.ID <= 0 || update.ID < offset {
				continue
			}
			now := time.Now()
			offset, lastUpdate = update.ID+1, now
			action := update.Message.command(chatID, now)
			if action == "" || time.Unix(update.Message.Date, 0).Before(activatedAt) {
				continue
			}
			// Record the command before acting so a poll retried after restart cannot enqueue it twice.
			state.UpdateID, state.UpdatedAt = update.ID, now
			if err := r.saveTelegramControl(state); err != nil {
				return err
			}
			var reply string
			var keyboard *telegramReplyKeyboard
			if action == "start" {
				reply, keyboard = welcome, telegramManualKeyboard
			} else {
				reply = r.requestTelegramManual(config, now)
			}
			if reply != "" && r.currentTelegramCommandConfig() == config {
				if err := sendTelegramMessage(ctx, client, settings, reply, keyboard); err != nil && ctx.Err() == nil {
					log.Printf("Telegram 手动同步回复发送失败：%s", err)
				}
			}
		}
	}
	return nil
}
