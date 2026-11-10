package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const telegramStateFile = "telegram.json"

var telegramToken = regexp.MustCompile(`^[1-9][0-9]{0,19}:[A-Za-z0-9_-]{1,128}$`)
var telegramUsername = regexp.MustCompile(`^@[A-Za-z0-9_]{1,32}$`)

type telegramSettings struct {
	Enabled    bool   `json:"enabled"`
	ChatID     string `json:"chat_id"`
	Token      string `json:"token"`
	ManualSync bool   `json:"manual_sync"`
}

type telegramPublic struct {
	Enabled    bool   `json:"enabled"`
	ChatID     string `json:"chat_id"`
	HasToken   bool   `json:"has_token"`
	ManualSync bool   `json:"manual_sync"`
}

func (settings telegramSettings) public() telegramPublic {
	return telegramPublic{Enabled: settings.Enabled, ChatID: settings.ChatID,
		HasToken: settings.Token != "", ManualSync: settings.ManualSync}
}

func (settings *telegramSettings) validate() error {
	settings.ChatID = strings.TrimSpace(settings.ChatID)
	settings.Token = strings.TrimSpace(settings.Token)
	if settings.Token != "" && !telegramToken.MatchString(settings.Token) {
		return errors.New("Telegram Bot Token 格式无效")
	}
	if settings.ChatID != "" && !telegramUsername.MatchString(settings.ChatID) {
		id, err := strconv.ParseInt(settings.ChatID, 10, 64)
		if err != nil || id == 0 {
			return errors.New("Chat ID 须为非零数字或 @用户名")
		}
	}
	if settings.Enabled && (settings.Token == "" || settings.ChatID == "") {
		return errors.New("启用 Telegram 通知前请填写 Bot Token 和 Chat ID")
	}
	if settings.ManualSync && settings.commandChatID() == 0 {
		return errors.New("启用 Telegram 手动同步须同时启用通知并填写正数私聊 Chat ID")
	}
	return nil
}

func (r *receiver) loadTelegram() error {
	path := filepath.Join(r.stateDir, telegramStateFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 2048 {
		return errors.New("Telegram 配置文件无效")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var settings *telegramSettings
	if err := json.Unmarshal(data, &settings); err != nil || settings == nil {
		return errors.New("Telegram 配置文件无效")
	}
	if err := settings.validate(); err != nil {
		return err
	}
	r.telegram = *settings
	return nil
}

func (r *receiver) getTelegram(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	public := r.telegram.public()
	r.mu.Unlock()
	jsonReply(w, http.StatusOK, public)
}

func (r *receiver) setTelegram(w http.ResponseWriter, req *http.Request) {
	if !isJSON(req) {
		jsonReply(w, http.StatusUnsupportedMediaType, result{Error: "仅支持 JSON 请求"})
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, req.Body, 2048))
	decoder.DisallowUnknownFields()
	var input *telegramSettings
	if err := decoder.Decode(&input); err != nil || input == nil || decoder.Decode(new(any)) != io.EOF {
		jsonReply(w, http.StatusBadRequest, result{Error: "Telegram 设置无效"})
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.TrimSpace(input.Token) == "" {
		input.Token = r.telegram.Token
	}
	if err := input.validate(); err != nil {
		jsonReply(w, http.StatusBadRequest, result{Error: err.Error()})
		return
	}
	data, err := json.Marshal(input)
	if err == nil {
		err = writeAtomic(r.stateDir, telegramStateFile, data)
	}
	if err != nil {
		jsonReply(w, http.StatusInternalServerError, result{Error: "无法保存 Telegram 设置"})
		return
	}
	if r.telegram != *input {
		r.telegram = *input
		r.telegramRevision++
	}
	jsonReply(w, http.StatusOK, r.telegram.public())
}

func newTelegramClient() *http.Client {
	return &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:                  http.ProxyFromEnvironment,
			DialContext:            (&net.Dialer{Timeout: 4 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:      true,
			TLSHandshakeTimeout:    4 * time.Second,
			IdleConnTimeout:        30 * time.Second,
			MaxIdleConns:           2,
			MaxResponseHeaderBytes: 16 << 10,
		},
	}
}

func sendTelegram(ctx context.Context, client *http.Client, settings telegramSettings, text string) error {
	return sendTelegramMessage(ctx, client, settings, text, nil)
}

func sendTelegramMessage(ctx context.Context, client *http.Client, settings telegramSettings, text string, keyboard *telegramReplyKeyboard) error {
	if settings.Token == "" || settings.ChatID == "" {
		return errors.New("请先保存 Bot Token 和 Chat ID")
	}
	var chat any = settings.ChatID
	if id, err := strconv.ParseInt(settings.ChatID, 10, 64); err == nil {
		chat = id
	}
	return callTelegram(ctx, client, settings.Token, "sendMessage", struct {
		ChatID      any                    `json:"chat_id"`
		Text        string                 `json:"text"`
		ParseMode   string                 `json:"parse_mode"`
		ReplyMarkup *telegramReplyKeyboard `json:"reply_markup,omitempty"`
	}{chat, text, "HTML", keyboard}, nil)
}

type telegramAPIError struct {
	code       int
	retryAfter time.Duration
}

func (e telegramAPIError) Error() string {
	switch e.code {
	case http.StatusUnauthorized:
		return "Telegram Bot Token 未通过验证"
	case http.StatusBadRequest, http.StatusForbidden:
		return "Telegram 无法处理此会话的请求，请检查 Chat ID 和 Bot 权限"
	case http.StatusConflict:
		return "Telegram Bot 已有 Webhook 或其他命令接收进程"
	case http.StatusTooManyRequests:
		return "Telegram 请求过于频繁，请稍后重试"
	default:
		return "Telegram 未确认请求成功"
	}
}

func callTelegram(ctx context.Context, client *http.Client, token, method string, input, output any) error {
	data, err := json.Marshal(input)
	if err != nil {
		return errors.New("无法准备 Telegram 请求")
	}
	endpoint := "https://api.telegram.org/bot" + token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return errors.New("无法准备 Telegram 请求")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// net/http errors can contain the request URL, including the Bot Token.
		return errors.New("无法连接 Telegram，请检查 NAS 网络后重试")
	}
	defer resp.Body.Close()
	var response struct {
		OK        bool            `json:"ok"`
		Result    json.RawMessage `json:"result"`
		ErrorCode int             `json:"error_code"`
		Params    struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&response); err != nil ||
		resp.StatusCode != http.StatusOK || !response.OK {
		code := resp.StatusCode
		if code == http.StatusOK {
			code = response.ErrorCode
		}
		return telegramAPIError{code, time.Duration(response.Params.RetryAfter) * time.Second}
	}
	if output != nil && json.Unmarshal(response.Result, output) != nil {
		return errors.New("Telegram 返回的数据无效")
	}
	return nil
}

func (r *receiver) testTelegram(w http.ResponseWriter, req *http.Request) {
	if !isJSON(req) {
		jsonReply(w, http.StatusUnsupportedMediaType, result{Error: "仅支持 JSON 请求"})
		return
	}
	r.mu.Lock()
	settings := r.telegram
	r.mu.Unlock()
	if settings.Token == "" || settings.ChatID == "" {
		jsonReply(w, http.StatusBadRequest, result{Error: "请先保存 Bot Token 和 Chat ID"})
		return
	}
	preview := telegramMessage{
		title: "🧪 bz-rsync · 测试通知", day: time.Now().In(beijing).Format(time.DateOnly),
		sizeLabel: "示例批次大小", totalBytes: 12_661_617_936,
		breakdown: archiveBreakdown{SiteBytes: 8_600_000_000, DatabaseBytes: 4_061_617_936},
		sample:    true,
		timeLabel: "示例耗时", timeValue: telegramDuration(4*time.Minute + 18*time.Second),
		detail: "这是通知样式预览，NAS 已连接 Telegram。",
	}
	if err := sendTelegram(req.Context(), r.telegramClient, settings, preview.format()); err != nil {
		jsonReply(w, http.StatusBadGateway, result{Error: err.Error()})
		return
	}
	jsonReply(w, http.StatusOK, result{OK: true})
}

type telegramNotice struct {
	settings telegramSettings
	message  telegramMessage
	day      string
	kind     string
}

type telegramMessage struct {
	title      string
	day        string
	sizeLabel  string
	totalBytes int64
	breakdown  archiveBreakdown
	sample     bool
	timeLabel  string
	timeValue  string
	detail     string
}

func telegramGB(size int64) string {
	return fmt.Sprintf("%.2f GB", float64(size)/1_000_000_000)
}

func (message telegramMessage) format() string {
	lines := []string{
		"<b>" + html.EscapeString(message.title) + "</b>",
		"📅 <b>批次日期</b>  " + html.EscapeString(message.day),
	}
	if message.totalBytes > 0 {
		lines = append(lines, "📦 <b>"+html.EscapeString(message.sizeLabel)+"</b>  "+telegramGB(message.totalBytes))
	}
	if message.breakdown.SiteBytes > 0 && message.breakdown.DatabaseBytes > 0 {
		prefix := ""
		if message.sample {
			prefix = "示例"
		}
		lines = append(lines,
			"🌐 <b>"+prefix+"网站归档大小</b>  "+telegramGB(message.breakdown.SiteBytes),
			"🗃 <b>"+prefix+"数据库归档大小</b>  "+telegramGB(message.breakdown.DatabaseBytes),
		)
	}
	if message.timeValue != "" {
		lines = append(lines, "⏱ <b>"+html.EscapeString(message.timeLabel)+"</b>  "+html.EscapeString(message.timeValue))
	}
	return strings.Join(append(lines, html.EscapeString(message.detail)), "\n")
}

func telegramDuration(duration time.Duration) string {
	if duration < time.Second {
		return "少于 1 秒"
	}
	seconds := int64(duration.Round(time.Second) / time.Second)
	if seconds >= 3600 {
		return fmt.Sprintf("%d 小时 %d 分 %d 秒", seconds/3600, seconds/60%60, seconds%60)
	}
	if seconds >= 60 {
		return fmt.Sprintf("%d 分 %d 秒", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%d 秒", seconds)
}

func (r *receiver) redactTelegramToken(line string) string {
	if r.telegram.Token != "" {
		return strings.ReplaceAll(line, r.telegram.Token, "[已隐藏]")
	}
	return line
}

func (r *receiver) finishRun(job *run, status, line string) {
	finalized := job.Status == "success" || job.Status == "failed"
	job.finish(status, r.redactTelegramToken(line))
	if finalized || !r.telegram.Enabled || (status != "success" && status != "failed") {
		return
	}
	day := "未知日期"
	if value, err := time.Parse(time.DateOnly, job.Day); err == nil {
		day = value.Format(time.DateOnly)
	}
	message := telegramMessage{day: day, totalBytes: job.TotalBytes, sizeLabel: "批次大小"}
	if job.Ended != nil && !job.Started.IsZero() && !job.Ended.Before(job.Started) {
		message.timeLabel = "任务耗时"
		message.timeValue = telegramDuration(job.Ended.Sub(job.Started))
	}
	if status == "success" {
		message.title = "✅ bz-rsync · 同步成功"
		message.detail = "SHA-256 校验通过，已入库。"
		job.ScheduledSuccessQueued = job.Scheduled
		r.telegramPending = append(r.telegramPending, telegramNotice{settings: r.telegram, message: message, day: day, kind: "同步成功"})
		return
	}
	// Use controlled stage names so arbitrary sender errors never leave the NAS.
	reason := "同步未完成"
	switch {
	case strings.HasPrefix(line, "同步失败："):
		reason = "发送端传输失败"
	case strings.HasPrefix(line, "校验或发布失败："):
		reason = "SHA-256 校验或归档发布失败"
	case strings.HasPrefix(line, "发送端长时间无响应"):
		reason = "发送端长时间无响应"
	case strings.HasPrefix(line, "接收端重启"):
		reason = "接收端重启时任务未完成"
	case strings.HasPrefix(line, "取消时清理暂存失败："):
		reason = "取消后清理暂存失败"
	}
	message.title = "❌ bz-rsync · 同步失败"
	message.sizeLabel = "计划批次大小"
	message.detail = reason + "，详情见同步历史。"
	r.telegramPending = append(r.telegramPending, telegramNotice{settings: r.telegram, message: message, day: day, kind: "同步失败"})
}

// queueManualResult handles outcomes that do not already have a transfer notification.
func (r *receiver) queueManualResult(request manualSync) {
	if !r.telegram.Enabled {
		return
	}
	hasRun, transferred := false, false
	for _, job := range r.runs {
		if job.RequestID == request.ID {
			hasRun = true
			transferred = transferred || job.Status == "success"
		}
	}
	message := telegramMessage{day: request.Day}
	var kind string
	switch {
	case request.Status == "success" && request.Review != nil && !transferred:
		review := request.Review
		kind = "手动复核"
		message.title = "✅ bz-rsync · 手动复核成功"
		message.sizeLabel, message.totalBytes = "批次大小", review.Sizes.TotalBytes
		message.breakdown = review.Sizes
		message.timeLabel, message.timeValue = "复核耗时", telegramDuration(review.Elapsed)
		message.detail = "SHA-256 复核通过，批次已入库。"
	case request.Status == "failed" && !hasRun:
		kind = "手动同步失败"
		message.title = "❌ bz-rsync · 手动同步失败"
		message.detail = "手动同步未完成，请在同步控制台查看原因。"
	default:
		return
	}
	r.queueTelegram(telegramNotice{settings: r.telegram, message: message, day: request.Day, kind: kind})
}

func (r *receiver) formatTelegramNotice(notice telegramNotice) string {
	if notice.kind == "同步成功" || notice.kind == "定时复核" {
		if breakdown, err := r.archivedBreakdown(notice.day); err == nil {
			notice.message.breakdown = breakdown
		}
	}
	return notice.message.format()
}

func (r *receiver) queueTelegram(notice telegramNotice) {
	select {
	case r.telegramQueue <- notice:
	default:
		log.Print("Telegram 通知队列已满，已跳过一条通知")
	}
}

func (r *receiver) queueTelegramResults() {
	for _, notice := range r.telegramPending {
		r.queueTelegram(notice)
	}
	r.telegramPending = nil
}

func (r *receiver) telegramWorker() {
	for notice := range r.telegramQueue {
		r.mu.Lock()
		current := r.telegram
		r.mu.Unlock()
		if !current.Enabled || notice.settings.ChatID != current.ChatID || notice.settings.Token != current.Token {
			continue
		}
		if err := sendTelegram(context.Background(), r.telegramClient, notice.settings, r.formatTelegramNotice(notice)); err != nil {
			log.Printf("Telegram 通知发送失败：%s", err)
		} else {
			log.Printf("Telegram 通知发送成功 · %s · %s", notice.day, notice.kind)
		}
	}
}
