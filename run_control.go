package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func (r *receiver) findRun(id string) *run {
	for i := range r.runs {
		if r.runs[i].ID == id {
			return &r.runs[i]
		}
	}
	return nil
}

func (r *receiver) latestDateRun(day string) *run {
	for i := range r.runs {
		if r.runs[i].Day == day {
			return &r.runs[i]
		}
	}
	return nil
}

func pendingManual(request *manualSync) bool {
	return request != nil && (request.Status == "queued" || request.Status == "running")
}

func (r *receiver) runAction(w http.ResponseWriter, req *http.Request) {
	if !isJSON(req) {
		jsonReply(w, 415, result{Error: "仅支持 JSON 请求"})
		return
	}
	var input struct {
		ID     string `json:"id"`
		Action string `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1024)).Decode(&input); err != nil {
		jsonReply(w, 400, result{Error: "同步控制请求无效"})
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	reply, err := r.applyRunAction(input.ID, input.Action)
	if err != nil {
		jsonReply(w, 409, result{Error: err.Error()})
		return
	}
	jsonReply(w, 200, reply)
}

func (r *receiver) applyRunAction(id, action string) (result, error) {
	job := r.findRun(id)
	if job == nil || r.latestDateRun(job.Day) != job {
		return result{}, fmt.Errorf("同步任务已变化，请刷新")
	}
	switch action {
	case "pause", "cancel":
		if job.Status == "paused" && action == "cancel" {
			if pendingManual(r.manual) && r.manual.ResumeID == job.ID {
				next := *r.manual
				next.Status, next.UpdatedAt = "cancelled", time.Now()
				if err := r.saveManual(next); err != nil {
					return result{}, err
				}
			}
			job.Control = action
			if err := r.save(); err != nil {
				return result{}, err
			}
			return result{OK: true}, r.completeStop(job)
		}
		if job.Status != "transferring" {
			return result{}, fmt.Errorf("仅传输中的任务支持此操作")
		}
		if job.Control == "cancel" && action == "pause" {
			return result{}, fmt.Errorf("任务已在取消")
		}
		job.Control = action
		if err := r.save(); err != nil {
			return result{}, err
		}
		if r.transfer == "" && r.ready == job.ID {
			return result{OK: true}, r.completeStop(job)
		}
		return result{OK: true}, nil
	case "resume":
		if job.Status != "paused" {
			return result{}, fmt.Errorf("只有已暂停的任务可以继续")
		}
		if r.retention.Days > 0 && job.Day < oldestDay(r.retention.Days) {
			return result{}, fmt.Errorf("该日期早于 NAS 保留范围")
		}
		if pendingManual(r.manual) {
			if r.manual.ResumeID == job.ID {
				return result{OK: true, Manual: r.manual}, nil
			}
			return result{}, fmt.Errorf("已有手动同步请求，请完成后再继续")
		}
		if err := r.queueManual(job.Day, job.ID, job.Manifest); err != nil {
			return result{}, err
		}
		return result{OK: true, Manual: r.manual}, nil
	default:
		return result{}, fmt.Errorf("未知同步操作")
	}
}

func (r *receiver) controlForRun(id string) (result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job := r.findRun(id)
	if job == nil {
		return result{}, fmt.Errorf("同步任务不存在")
	}
	action := job.Control
	if job.Status == "paused" {
		action = "pause"
	}
	if job.Status == "cancelled" {
		action = "cancel"
	}
	return result{OK: true, Action: action}, nil
}

func (r *receiver) checkRunRequest(c command) error {
	var manual *manualSync
	if c.RequestID != "" {
		manual = r.manual
		if manual == nil || manual.ID != c.RequestID || manual.Day != c.Day || manual.Status != "running" {
			return fmt.Errorf("手动同步请求已失效")
		}
		if manual.Manifest != "" && manual.Manifest != c.Manifest {
			return fmt.Errorf("备份来源已变更，无法继续原批次")
		}
	}
	prior := r.latestDateRun(c.Day)
	if prior == nil {
		return nil
	}
	switch prior.Status {
	case "paused":
		if manual == nil || manual.ResumeID != prior.ID || manual.Manifest != prior.Manifest {
			return fmt.Errorf("该批次已暂停，请在控制台点击继续")
		}
	case "cancelled":
		if manual == nil {
			return fmt.Errorf("该日期已取消，可在控制台发起新的手动同步")
		}
	}
	return nil
}

func (r *receiver) completeStop(job *run) error {
	if r.ready == job.ID {
		r.ready = ""
	}
	status, line := "paused", "任务已暂停，已接收的暂存文件可用于继续同步"
	var cleanupErr error
	if job.Control == "cancel" {
		cleanupErr = r.discardStage(job)
		status, line = "cancelled", "任务已取消，NAS 上本批次的暂存文件已删除"
		if cleanupErr != nil {
			status, line = "failed", "取消时清理暂存失败："+cleanupErr.Error()
		}
	}
	job.Control = ""
	r.finishRun(job, status, line)
	historyErr := r.save()
	var manualErr error
	if pendingManual(r.manual) && r.manual.ID == job.RequestID {
		next := *r.manual
		next.Status, next.UpdatedAt = status, time.Now()
		if cleanupErr != nil {
			next.Error = line
		}
		manualErr = r.saveManual(next)
	}
	return errors.Join(cleanupErr, historyErr, manualErr)
}

func (r *receiver) discardStage(job *run) error {
	day, err := time.Parse(time.DateOnly, job.Day)
	if err != nil || day.Format(time.DateOnly) != job.Day || !digestPattern.MatchString(job.Manifest) {
		return fmt.Errorf("无法确认暂存目录归属")
	}
	root, err := openSplitBatchDir(filepath.Dir(r.staging(job.Day)))
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(job.Day)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() {
		return fmt.Errorf("暂存路径不是实际目录")
	}
	name := job.Day + ".manifest"
	info, err = root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("缺少可核对的暂存清单")
	}
	digest, err := root.ReadFile(name)
	if err != nil || string(digest) != job.Manifest {
		return fmt.Errorf("暂存清单与取消请求不一致")
	}
	if err := root.RemoveAll(job.Day); err != nil {
		return err
	}
	return root.Remove(name)
}
