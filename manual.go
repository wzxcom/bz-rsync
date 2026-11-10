package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type manualSync struct {
	ID          string    `json:"id"`
	Day         string    `json:"day"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	ResumeID    string    `json:"resume_id,omitempty"`
	Manifest    string    `json:"manifest_sha256,omitempty"`
	RequestedAt time.Time `json:"requested_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (r *receiver) loadManual() error {
	data, err := os.ReadFile(filepath.Join(r.stateDir, "manual-sync.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &r.manual)
}

func (r *receiver) saveManual(next manualSync) error {
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := writeAtomic(r.stateDir, "manual-sync.json", data); err != nil {
		return err
	}
	r.manual = &next
	return nil
}

func (r *receiver) requestManual(w http.ResponseWriter, req *http.Request) {
	if !isJSON(req) {
		jsonReply(w, 415, result{Error: "仅支持 JSON 请求"})
		return
	}
	var input struct {
		Day string `json:"day"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1024)).Decode(&input); err != nil {
		jsonReply(w, 400, result{Error: "同步请求格式无效"})
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	status, reply := r.submitManualLocked(input.Day)
	jsonReply(w, status, reply)
}

// submitManualLocked validates and submits a manual request with r.mu held.
func (r *receiver) submitManualLocked(day string) (int, result) {
	if parsed, err := time.Parse(time.DateOnly, day); err != nil || parsed.Format(time.DateOnly) != day ||
		day > time.Now().In(beijing).Format(time.DateOnly) {
		return 400, result{Error: "请选择有效且不晚于今天的备份日期"}
	}
	if _, err := os.Stat(filepath.Join(r.stateDir, "authorized_keys")); err != nil {
		return 409, result{Error: "请先配对宝塔发送端"}
	}
	if r.retention.Days > 0 && day < oldestDay(r.retention.Days) {
		return 409, result{Error: "该日期早于 NAS 保留范围"}
	}
	if pendingManual(r.manual) {
		if r.manual.Day == day {
			return 200, result{OK: true, Manual: r.manual}
		}
		return 409, result{Error: "已有手动同步请求，请完成后再提交"}
	}
	if prior := r.latestDateRun(day); prior != nil && prior.Status == "paused" {
		return 409, result{Error: "该日期有已暂停的任务，请使用继续按钮"}
	}
	if err := r.queueManual(day, "", ""); err != nil {
		return 500, result{Error: "无法保存手动同步请求"}
	}
	return 200, result{OK: true, Manual: r.manual}
}

func (r *receiver) queueManual(day, resumeID, manifest string) error {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	now := time.Now()
	return r.saveManual(manualSync{ID: hex.EncodeToString(id[:]), Day: day,
		Status: "queued", RequestedAt: now, UpdatedAt: now, ResumeID: resumeID, Manifest: manifest})
}

func (r *receiver) claimManual() (result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !pendingManual(r.manual) {
		return result{OK: true}, nil
	}
	for _, job := range r.runs {
		if job.Status == "transferring" || job.Status == "verifying" {
			return result{OK: true}, nil
		}
	}
	// The sender serializes claims and scheduled runs with one local lock.
	// A new poll can resume a request whose previous sender process exited.
	next := *r.manual
	next.Status, next.UpdatedAt = "running", time.Now()
	if err := r.saveManual(next); err != nil {
		return result{}, err
	}
	return result{OK: true, Manual: r.manual}, nil
}

func (r *receiver) reportManual(c command) (result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.manual != nil && r.manual.ID == c.RequestID && (r.manual.Status == "paused" || r.manual.Status == "cancelled") {
		return result{OK: true}, nil
	}
	if r.manual == nil || r.manual.ID != c.RequestID || r.manual.Status != "running" || c.ConfigOK == nil {
		return result{}, fmt.Errorf("手动同步请求不存在或已完成")
	}
	next := *r.manual
	next.UpdatedAt = time.Now()
	if *c.ConfigOK {
		next.Status, next.Error = "success", ""
	} else {
		next.Status, next.Error = "failed", cleanLine(c.Error, 500)
		if next.Error == "" {
			next.Error = "宝塔发送端未能完成该日期的同步"
		}
	}
	return result{OK: true}, r.saveManual(next)
}
