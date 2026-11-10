package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type event struct {
	Time time.Time `json:"time"`
	Line string    `json:"line"`
}

type run struct {
	ID                     string     `json:"id"`
	Day                    string     `json:"day"`
	Status                 string     `json:"status"`
	TotalBytes             int64      `json:"total_bytes"`
	Bytes                  int64      `json:"bytes"`
	Percent                int        `json:"percent"`
	Speed                  string     `json:"speed"`
	ETA                    string     `json:"eta"`
	Started                time.Time  `json:"started"`
	Updated                time.Time  `json:"updated"`
	Ended                  *time.Time `json:"ended,omitempty"`
	Events                 []event    `json:"events"`
	Manifest               string     `json:"manifest_sha256"`
	Control                string     `json:"control,omitempty"`
	RequestID              string     `json:"request_id,omitempty"`
	StageMode              string     `json:"stage_mode,omitempty"`
	Scheduled              bool       `json:"scheduled,omitempty"`
	ScheduledSuccessQueued bool       `json:"scheduled_success_queued,omitempty"`
}

const scheduledConfirmationFile = "scheduled_confirmation.json"

type scheduledConfirmation struct {
	Day        string    `json:"day"`
	CheckedAt  time.Time `json:"checked_at"`
	TotalBytes int64     `json:"total_bytes"`
}

type command struct {
	RunID      string `json:"run_id"`
	Day        string `json:"day"`
	TotalBytes int64  `json:"total_bytes"`
	Bytes      int64  `json:"bytes"`
	Percent    int    `json:"percent"`
	Speed      string `json:"speed"`
	ETA        string `json:"eta"`
	Error      string `json:"error"`
	Manifest   string `json:"manifest_sha256"`
	Content    string `json:"content_sha256"`
	Revision   uint64 `json:"revision"`
	ConfigOK   *bool  `json:"ok"`
	RequestID  string `json:"request_id"`
	Token      string `json:"token"`
	PublicKey  string `json:"public_key"`
	KeyID      string `json:"key_id"`
	StageMode  string `json:"stage_mode"`
	Scheduled  bool   `json:"scheduled"`
}

type result struct {
	OK          bool        `json:"ok"`
	RunID       string      `json:"run_id,omitempty"`
	AlreadyDone bool        `json:"already_done,omitempty"`
	Directory   string      `json:"directory,omitempty"`
	Error       string      `json:"error,omitempty"`
	Manual      *manualSync `json:"manual_sync,omitempty"`
	Action      string      `json:"action,omitempty"`
	*SourceSelection
}

type receiver struct {
	mu                   sync.Mutex
	pathMu               sync.RWMutex
	paths                pathConfig
	sources              sourceSettings
	manual               *manualSync
	runs                 []run
	stateDir             string
	backupDir            string
	split                bool
	transfer             string
	ready                string
	retention            retention
	cleanup              chan struct{}
	telegram             telegramSettings
	telegramClient       *http.Client
	telegramQueue        chan telegramNotice
	telegramPending      []telegramNotice
	scheduled            *scheduledConfirmation
	scheduledUnavailable bool
}

func openReceiver(stateDir, backupDir string) (*receiver, error) {
	return openReceiverLayout(stateDir, backupDir, false)
}

func openReceiverLayout(stateDir, backupDir string, split bool) (*receiver, error) {
	stage := filepath.Join(backupDir, ".bz-rsync-staging")
	if err := ensureDirectory(stage, 0700); err != nil {
		return nil, err
	}
	r := &receiver{
		runs: []run{}, stateDir: stateDir, backupDir: backupDir, split: split, cleanup: make(chan struct{}, 1),
		telegramClient: newTelegramClient(), telegramQueue: make(chan telegramNotice, 64),
	}
	if err := r.loadTelegram(); err != nil {
		log.Print("Telegram 配置读取失败，通知暂不可用")
	}
	if err := r.loadScheduledConfirmation(); err != nil {
		r.scheduledUnavailable = true
		log.Printf("定时复核记录读取失败 · %v", err)
	}
	if err := r.loadPaths(); err != nil {
		return nil, err
	}
	if err := r.loadSources(); err != nil {
		return nil, err
	}
	if err := r.loadManual(); err != nil {
		return nil, err
	}
	if split {
		pair := r.paths.Current
		for _, path := range []string{filepath.Join(backupDir, pair.Site), filepath.Join(backupDir, pair.Database), filepath.Join(stage, ".published")} {
			if err := ensureDirectory(path, 0750); err != nil {
				return nil, err
			}
		}
	}
	if err := r.loadRetention(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "history.json"))
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &r.runs); err != nil {
		return nil, err
	}
	if r.runs == nil {
		r.runs = []run{}
	}
	changed, recovered := false, false
	for i := range r.runs {
		job := &r.runs[i]
		if job.Control != "" && (job.Status == "transferring" || job.Status == "paused") {
			if err := r.completeStop(job); err != nil {
				log.Printf("recover stop: %v", err)
			}
			changed = true
			continue
		}
		recoveredPublish := job.Status == "verifying" && r.verifyPublished(job.Day, job.Manifest) == nil
		if split && !recoveredPublish && r.splitPending(job.Day, job.Manifest) {
			recoveredPublish = r.publishSplit(job.Day, job.Manifest) == nil
		}
		switch {
		case recoveredPublish:
			r.finishRun(job, "success", "接收端恢复，已确认批次通过校验并发布")
			r.removePublishMarker(job.Day)
			changed = true
		case job.Status == "verifying" || job.Status == "transferring":
			r.finishRun(job, "failed", "接收端重启，未完成批次可由发送端重试")
			changed = true
		}
		if job.Status == "success" && r.retention.Managed[job.Day] == "" &&
			r.matchesPublished(job.Day, job.Manifest) {
			r.retention.Managed[job.Day] = job.Manifest
			recovered = true
		}
	}
	if recovered {
		if err := r.saveRetention(); err != nil {
			return nil, err
		}
	}
	if changed {
		return r, r.save()
	}
	return r, nil
}

func writeAtomic(dir, name string, data []byte) error {
	file, err := os.CreateTemp(dir, ".bz-rsync-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), filepath.Join(dir, name))
}

func (r *receiver) save() error {
	data, err := json.Marshal(r.runs)
	if err != nil {
		return err
	}
	if err := writeAtomic(r.stateDir, "history.json", data); err != nil {
		return err
	}
	r.queueTelegramResults()
	return nil
}

func (r *receiver) loadScheduledConfirmation() error {
	path := filepath.Join(r.stateDir, scheduledConfirmationFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024 {
		return errors.New("定时复核记录文件无效")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var record scheduledConfirmation
	if err := json.Unmarshal(data, &record); err != nil {
		return errors.New("定时复核记录格式无效")
	}
	day, err := time.Parse(time.DateOnly, record.Day)
	if err != nil || day.Format(time.DateOnly) != record.Day || record.CheckedAt.IsZero() || record.TotalBytes < 1 {
		return errors.New("定时复核记录内容无效")
	}
	r.scheduled = &record
	return nil
}

func (r *receiver) recordScheduledConfirmation(c command) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.scheduledUnavailable {
		return errors.New("定时复核记录文件需要检查")
	}
	if r.scheduled != nil && c.Day <= r.scheduled.Day {
		return nil
	}
	record := scheduledConfirmation{Day: c.Day, CheckedAt: time.Now().In(beijing), TotalBytes: c.TotalBytes}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := writeAtomic(r.stateDir, scheduledConfirmationFile, data); err != nil {
		return err
	}
	r.scheduled = &record
	log.Printf("批次 %s 定时复核通过，批次已入库", record.Day)
	if !r.telegram.Enabled {
		return nil
	}
	for _, job := range r.runs {
		if job.Day == c.Day && (job.ScheduledSuccessQueued || (job.Scheduled && job.Status == "verifying")) {
			return nil
		}
	}
	message := telegramMessage{
		title: "🔎 bz-rsync · 定时复核", day: record.Day, sizeLabel: "批次大小", totalBytes: record.TotalBytes,
		timeLabel: "复核时间", timeValue: record.CheckedAt.Format("2006-01-02 15:04:05") + "（北京时间）",
		detail: "定时复核通过，批次已入库。",
	}
	r.queueTelegram(telegramNotice{settings: r.telegram, message: message, day: record.Day, kind: "定时复核"})
	return nil
}

func (job *run) append(line string) {
	job.Events = append(job.Events, event{Time: time.Now(), Line: line})
	if len(job.Events) > 80 {
		job.Events = job.Events[len(job.Events)-80:]
	}
}

func (job *run) finish(status, line string) {
	now := time.Now()
	job.Status, job.Ended, job.Updated = status, &now, now
	job.Speed, job.ETA = "", ""
	if status == "success" {
		job.Percent, job.Bytes = 100, job.TotalBytes
	}
	job.append(line)
}

func (r *receiver) start(c command) (result, error) {
	day, err := time.Parse(time.DateOnly, c.Day)
	if err != nil || day.Format(time.DateOnly) != c.Day || !digestPattern.MatchString(c.Manifest) || c.TotalBytes < 1 {
		return result{}, fmt.Errorf("批次日期、清单摘要或大小无效")
	}
	if c.StageMode != "" && c.StageMode != "inplace" {
		return result{}, fmt.Errorf("暂存传输模式无效")
	}
	if present, err := r.publishedExists(c.Day); err != nil {
		return result{}, err
	} else if present {
		if err := r.verifyExisting(c.Day, c.Manifest, c.Content); err != nil {
			return result{}, fmt.Errorf("目标日期已有数据，校验不匹配或目录冲突：%w", err)
		}
		if c.Scheduled && c.RequestID == "" {
			if err := r.recordScheduledConfirmation(c); err != nil {
				log.Printf("批次 %s 定时复核记录保存失败 · %v", c.Day, err)
			}
		}
		return result{OK: true, AlreadyDone: true}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.retention.Days > 0 && c.Day < oldestDay(r.retention.Days) {
		return result{}, fmt.Errorf("批次日期早于 NAS 当前保留范围")
	}
	for i := range r.runs {
		job := &r.runs[i]
		if job.Status == "verifying" || r.transfer != "" {
			return result{}, fmt.Errorf("接收端正在处理另一个批次")
		}
		if job.Status == "transferring" {
			if time.Since(job.Updated) < 3*time.Minute {
				return result{}, fmt.Errorf("接收端有未结束任务，请稍后重试")
			}
			if job.Control != "" {
				if err := r.completeStop(job); err != nil {
					return result{}, err
				}
			} else {
				r.finishRun(job, "failed", "发送端长时间无响应，等待重试")
				r.ready = ""
			}
		}
	}
	if err := r.checkRunRequest(c); err != nil {
		return result{}, err
	}

	if present, err := r.publishedExists(c.Day); err != nil || present {
		return result{}, fmt.Errorf("目标目录刚发生变化，请重新发起任务")
	}
	if pending, err := exists(r.staging(c.Day) + ".publish"); r.split && (err != nil || pending) {
		return result{}, fmt.Errorf("此批次有待恢复的发布记录，请检查 NAS 日志并重启接收端")
	}
	stage := r.staging(c.Day)
	if err := os.Mkdir(stage, 0750); err != nil && !errors.Is(err, os.ErrExist) {
		return result{}, err
	}
	if info, err := os.Lstat(stage); err != nil || !info.IsDir() {
		return result{}, fmt.Errorf("暂存目录无效")
	}
	// A changed source manifest requires a clean transfer. Keep prior data isolated.
	meta := stage + ".manifest"
	old, err := os.ReadFile(meta)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result{}, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return result{}, err
	}
	runID := hex.EncodeToString(id[:])
	if string(old) != c.Manifest {
		entries, err := os.ReadDir(stage)
		if err != nil {
			return result{}, err
		}
		if len(entries) != 0 {
			if err := os.Rename(stage, stage+".conflict-"+runID); err != nil {
				return result{}, err
			}
			if err := os.Mkdir(stage, 0750); err != nil {
				return result{}, err
			}
		}
		if err := writeAtomic(filepath.Dir(meta), filepath.Base(meta), []byte(c.Manifest)); err != nil {
			return result{}, err
		}
	}
	now := time.Now()
	job := run{ID: runID, Day: c.Day, Status: "transferring", TotalBytes: c.TotalBytes,
		RequestID: c.RequestID, StageMode: c.StageMode,
		Scheduled: c.Scheduled && c.RequestID == "",
		Started:   now, Updated: now, Manifest: c.Manifest}
	job.append("发送端已连接，正在扫描并准备接收备份")
	previous := r.runs
	r.runs = append([]run{job}, previous...)
	if len(r.runs) > 60 {
		r.runs = r.runs[:60]
	}
	if err := r.save(); err != nil {
		r.runs = previous
		return result{}, err
	}
	log.Printf("批次 %s 开始接收 · %s", c.Day, runID)
	return result{OK: true, RunID: runID}, nil
}

func (r *receiver) staging(day string) string {
	return filepath.Join(r.backupDir, ".bz-rsync-staging", day)
}

func (r *receiver) active(id string) *run {
	for i := range r.runs {
		if r.runs[i].ID == id && r.runs[i].Status == "transferring" {
			return &r.runs[i]
		}
	}
	return nil
}

func cleanLine(value string, length int) string {
	runes := []rune(value)
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, string(runes[:min(len(runes), length)]))
}

func (r *receiver) execute(action string, c command) (result, error) {
	switch action {
	case "bz-finish":
		return r.finish(c.RunID)
	case "bz-start":
		return r.start(c)
	case "bz-ping":
		return result{OK: true}, nil
	case "bz-config":
		return r.senderSources()
	case "bz-config-result":
		return r.reportSources(c)
	case "bz-manual-claim":
		return r.claimManual()
	case "bz-manual-result":
		return r.reportManual(c)
	case "bz-enroll":
		return r.enroll(c)
	case "bz-run-control":
		return r.controlForRun(c.RunID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	job := r.active(c.RunID)
	if job == nil {
		prior := r.findRun(c.RunID)
		if action == "bz-fail" && prior != nil && (prior.Status == "paused" || prior.Status == "cancelled") {
			return result{OK: true}, nil
		}
		return result{}, fmt.Errorf("未找到可操作的同步任务")
	}
	switch action {
	case "claim":
		if r.transfer != "" || r.ready != "" {
			return result{}, fmt.Errorf("当前批次已经占用接收通道")
		}
		if job.Control != "" {
			err := r.completeStop(job)
			return result{}, errors.Join(fmt.Errorf("接收已暂停或取消"), err)
		}
		if job.StageMode == "inplace" {
			if err := adoptLegacyPartials(r.staging(job.Day)); err != nil {
				return result{}, err
			}
		}
		r.transfer = job.ID
		job.Updated = time.Now()
		job.append("rsync 加密接收开始")
		return result{OK: true, Directory: r.staging(job.Day)}, nil
	case "release":
		if r.transfer != job.ID {
			return result{}, fmt.Errorf("当前批次未占用接收通道")
		}
		r.transfer = ""
		if job.Control != "" {
			return result{OK: true}, r.completeStop(job)
		}
		if c.Error == "" {
			r.ready = job.ID
			job.append("rsync 数据接收完成，等待发送端确认")
		}
		job.Updated = time.Now()
	case "bz-progress":
		previous := job.Percent
		job.Percent = max(job.Percent, min(max(c.Percent, 0), 99))
		job.Bytes = max(job.Bytes, min(max(c.Bytes, 0), job.TotalBytes))
		job.Speed, job.ETA = cleanLine(c.Speed, 40), cleanLine(c.ETA, 40)
		job.Updated = time.Now()
		if previous/5 < job.Percent/5 {
			job.append(fmt.Sprintf("传输进度 %d%% · %s", job.Percent, job.Speed))
		}
	case "bz-fail":
		if r.transfer == job.ID {
			return result{}, fmt.Errorf("接收通道尚未退出")
		}
		if job.Control != "" {
			return result{OK: true}, r.completeStop(job)
		}
		r.ready = ""
		failure := cleanLine(r.redactTelegramToken(c.Error), 300)
		r.finishRun(job, "failed", "同步失败："+failure)
		log.Printf("批次 %s 同步失败 · %s", job.Day, failure)
	default:
		return result{}, fmt.Errorf("未知命令")
	}
	return result{OK: true}, r.save()
}

func (r *receiver) finish(id string) (result, error) {
	r.mu.Lock()
	job := r.active(id)
	if job == nil || r.transfer != "" || r.ready != id {
		r.mu.Unlock()
		return result{}, fmt.Errorf("rsync 接收尚未完成")
	}
	if job.Control != "" {
		err := r.completeStop(job)
		r.mu.Unlock()
		return result{}, errors.Join(fmt.Errorf("接收已暂停或取消"), err)
	}
	job.Status = "verifying"
	job.Speed, job.ETA = "", ""
	job.append("接收完成，正在逐个核对 SHA-256")
	day, digest := job.Day, job.Manifest
	if err := r.save(); err != nil {
		job.Status = "transferring"
		r.mu.Unlock()
		return result{}, err
	}
	r.ready = ""
	r.mu.Unlock()

	stage := r.staging(day)
	err := r.publish(day, digest)
	if err == nil {
		r.removePublishMarker(day)
		if removeErr := os.Remove(stage + ".manifest"); removeErr != nil {
			log.Printf("remove stage metadata: %v", removeErr)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// start is blocked while this run is verifying, so the slice remains stable.
	if err != nil {
		r.finishRun(job, "failed", "校验或发布失败："+err.Error())
		log.Printf("批次 %s 校验或发布失败 · %v", day, err)
	} else {
		r.finishRun(job, "success", "SHA-256 校验通过，日期目录已发布")
		log.Printf("批次 %s SHA-256 校验通过，日期目录已发布", day)
		r.retention.Managed[day] = digest
		if receiptErr := r.saveRetention(); receiptErr != nil {
			job.append("保留策略登记写入失败，接收端将重试")
			log.Printf("save retention receipt: %v", receiptErr)
		}
		r.scheduleCleanup()
	}
	if saveErr := r.save(); saveErr != nil {
		return result{}, saveErr
	}
	return result{OK: err == nil}, err
}

func (r *receiver) removePublishMarker(day string) {
	if r.split {
		if err := os.Remove(r.staging(day) + ".publish"); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("remove publish marker: %v", err)
		}
	}
}

func (r *receiver) expireInactive() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.transfer != "" {
		return
	}
	for i := range r.runs {
		job := &r.runs[i]
		if job.Status == "transferring" && time.Since(job.Updated) >= 3*time.Minute {
			if job.Control != "" {
				if err := r.completeStop(job); err != nil {
					log.Printf("complete stop: %v", err)
				}
				return
			}
			r.finishRun(job, "failed", "发送端长时间无响应，等待重试")
			r.ready = ""
			if err := r.save(); err != nil {
				log.Printf("save inactive run: %v", err)
			}
			return
		}
	}
}

func validPublicKey(key string) (string, error) {
	parts := strings.Fields(key)
	if len(parts) < 2 || parts[0] != "ssh-ed25519" || !validEd25519(parts[1]) {
		return "", fmt.Errorf("请粘贴有效的 ssh-ed25519 公钥")
	}
	return "restrict " + parts[0] + " " + parts[1] + "\n", nil
}
