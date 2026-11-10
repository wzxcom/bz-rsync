package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
	_ "time/tzdata"
)

type retention struct {
	Days         int               `json:"retention_days"`
	LastCleanup  *time.Time        `json:"last_cleanup,omitempty"`
	CleanupError string            `json:"cleanup_error,omitempty"`
	Managed      map[string]string `json:"managed"`
}

var beijing = func() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic(err)
	}
	return location
}()

func oldestDay(days int) string {
	return time.Now().In(beijing).AddDate(0, 0, 1-days).Format(time.DateOnly)
}

func (r *receiver) loadRetention() error {
	r.retention.Managed = make(map[string]string)
	data, err := os.ReadFile(filepath.Join(r.stateDir, "retention.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &r.retention); err != nil {
		return err
	}
	if r.retention.Managed == nil || r.retention.Days < 0 || r.retention.Days > 365 {
		return fmt.Errorf("保留设置或备份登记无效")
	}
	return nil
}

func (r *receiver) saveRetention() error {
	data, err := json.Marshal(r.retention)
	if err != nil {
		return err
	}
	return writeAtomic(r.stateDir, "retention.json", data)
}

func (r *receiver) setRetention(w http.ResponseWriter, req *http.Request) {
	if !isJSON(req) {
		jsonReply(w, 415, result{Error: "仅支持 JSON 请求"})
		return
	}
	var input struct {
		Days *int `json:"days"`
	}
	err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1024)).Decode(&input)
	if err != nil || input.Days == nil || *input.Days < 0 || *input.Days > 365 {
		jsonReply(w, 400, result{Error: "保留天数须为 0 到 365 的整数，0 表示关闭"})
		return
	}
	r.mu.Lock()
	previous := r.retention.Days
	r.retention.Days = *input.Days
	err = r.saveRetention()
	if err != nil {
		r.retention.Days = previous
	}
	r.mu.Unlock()
	if err != nil {
		log.Print(err)
		jsonReply(w, 500, result{Error: "无法保存保留设置"})
		return
	}
	r.scheduleCleanup()
	jsonReply(w, 200, result{OK: true})
}

func (r *receiver) scheduleCleanup() {
	select {
	case r.cleanup <- struct{}{}:
	default:
	}
}

func (r *receiver) retentionWorker() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	inactive := time.NewTicker(30 * time.Second)
	defer inactive.Stop()
	for {
		select {
		case <-r.cleanup:
		case <-ticker.C:
		case <-inactive.C:
			r.expireInactive()
			continue
		}
		r.cleanupExpired()
	}
}

func (r *receiver) cleanupExpired() {
	r.mu.Lock()
	if r.retention.Days == 0 {
		r.mu.Unlock()
		return
	}
	managed := make(map[string]string)
	cutoff := oldestDay(r.retention.Days)
	for day, digest := range r.retention.Managed {
		if day < cutoff {
			managed[day] = digest
		}
	}
	r.mu.Unlock()

	var firstError error
	for day, digest := range managed {
		if err := r.removeExpired(day, digest); err != nil {
			log.Printf("retention %s: %v", day, err)
			if firstError == nil {
				firstError = fmt.Errorf("%s：%w", day, err)
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.retention.LastCleanup = &now
	r.retention.CleanupError = ""
	if firstError != nil {
		r.retention.CleanupError = firstError.Error()
	}
	if err := r.saveRetention(); err != nil {
		log.Printf("save retention: %v", err)
	}
}

func (r *receiver) removeExpired(day, digest string) error {
	if _, err := time.Parse(time.DateOnly, day); err != nil || !digestPattern.MatchString(digest) {
		return fmt.Errorf("备份登记无效")
	}
	if r.split {
		return r.removeExpiredSplit(day, digest)
	}
	path := filepath.Join(r.backupDir, day)
	trash := r.staging(day) + ".trash-" + digest
	_, trashErr := os.Lstat(trash)
	if trashErr != nil && !errors.Is(trashErr, os.ErrNotExist) {
		return trashErr
	}
	_, pathErr := os.Lstat(path)
	if pathErr != nil && !errors.Is(pathErr, os.ErrNotExist) {
		return pathErr
	}
	if trashErr == nil && pathErr == nil {
		return fmt.Errorf("清理暂存和日期目录同时存在，需要检查")
	}
	if pathErr == nil {
		if err := verifyBatch(path, digest); err != nil {
			return fmt.Errorf("日期目录与入库清单不一致，保留目录：%w", err)
		}
	}

	r.mu.Lock()
	if r.retention.Days == 0 || day >= oldestDay(r.retention.Days) || r.retention.Managed[day] != digest {
		r.mu.Unlock()
		return nil
	}
	if pathErr == nil {
		if err := os.Rename(path, trash); err != nil {
			r.mu.Unlock()
			return err
		}
	}
	r.mu.Unlock()
	if err := os.RemoveAll(trash); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.retention.Managed, day)
	return r.saveRetention()
}

func (r *receiver) removeExpiredSplit(day, digest string) error {
	site, database := r.layoutDate("site", day), r.layoutDate("database", day)
	meta := r.splitMeta(day)
	trash := r.staging(day) + ".split-trash-" + digest
	ready := trash + ".ready"
	trashExists, err := exists(trash)
	if err != nil {
		return err
	}
	readyExists, err := exists(ready)
	if err != nil {
		return err
	}
	metaExists, err := exists(meta)
	if err != nil {
		return err
	}
	if metaExists && (trashExists || readyExists) {
		return fmt.Errorf("入库记录与清理暂存冲突")
	}

	paths := [2]string{site, database}
	if !readyExists {
		switch {
		case trashExists:
			paths, err = splitPaths(trash, site, database)
			if err == nil {
				err = verifySplitBatch(paths[0], paths[1], trash, digest)
			}
		case metaExists:
			err = verifySplitBatch(site, database, meta, digest)
		default:
			present, checkErr := r.publishedExists(day)
			if present || checkErr != nil {
				return fmt.Errorf("日期目录缺少入库记录，保留原目录")
			}
		}
		if err != nil {
			return fmt.Errorf("日期目录与入库清单不一致，保留目录：%w", err)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.retention.Days == 0 || day >= oldestDay(r.retention.Days) || r.retention.Managed[day] != digest {
		return nil
	}
	if metaExists {
		if err := moveNew(meta, trash); err != nil {
			return err
		}
		trashExists = true
	}
	if !readyExists && trashExists {
		for i, group := range []string{"site", "database"} {
			target := filepath.Join(trash, group)
			if paths[i] != target {
				if err := moveNew(paths[i], target); err != nil {
					return err
				}
			}
		}
		if err := writeAtomic(filepath.Dir(ready), filepath.Base(ready), []byte(digest)); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(trash); err != nil {
		return err
	}
	if err := os.Remove(ready); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(r.retention.Managed, day)
	return r.saveRetention()
}
