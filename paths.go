package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type archivePaths struct {
	Site     string `json:"site_dir"`
	Database string `json:"database_dir"`
}

type pathConfig struct {
	Current archivePaths            `json:"current"`
	Days    map[string]archivePaths `json:"days"`
}

var directoryName = regexp.MustCompile("^[\\p{L}\\p{N}][\\p{L}\\p{N}_-]{0,63}$")

func (p archivePaths) valid() bool {
	return directoryName.MatchString(p.Site) && directoryName.MatchString(p.Database) &&
		!strings.EqualFold(p.Site, p.Database)
}

func (r *receiver) loadPaths() error {
	r.paths = pathConfig{Current: archivePaths{"site", "database"}, Days: map[string]archivePaths{}}
	data, err := os.ReadFile(filepath.Join(r.stateDir, "paths.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(data, &r.paths); err != nil {
			return err
		}
	}
	if !r.paths.Current.valid() || r.paths.Days == nil {
		return fmt.Errorf("接收路径配置无效")
	}
	for _, paths := range r.paths.Days {
		if !paths.valid() {
			return fmt.Errorf("已有批次的路径配置无效")
		}
	}
	return nil
}

func (r *receiver) archivePaths(day string) archivePaths {
	r.pathMu.RLock()
	defer r.pathMu.RUnlock()
	if pair, exists := r.paths.Days[day]; exists {
		return pair
	}
	return r.paths.Current
}

// savePaths commits the configuration before making it visible to writers.
func (r *receiver) savePaths(next pathConfig) error {
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := writeAtomic(r.stateDir, "paths.json", data); err != nil {
		return err
	}
	r.paths = next
	return nil
}

func copyArchiveDays(days map[string]archivePaths) map[string]archivePaths {
	copy := make(map[string]archivePaths, len(days)+1)
	for day, paths := range days {
		copy[day] = paths
	}
	return copy
}

func (r *receiver) pinPaths(day string) error {
	r.pathMu.Lock()
	defer r.pathMu.Unlock()
	if _, exists := r.paths.Days[day]; exists {
		return nil
	}
	next := pathConfig{r.paths.Current, copyArchiveDays(r.paths.Days)}
	next.Days[day] = r.paths.Current
	return r.savePaths(next)
}

type pathView struct {
	archivePaths
	Editable        bool     `json:"editable"`
	BackupRoot      string   `json:"backup_root"`
	SitePath        string   `json:"site_path"`
	DatabasePath    string   `json:"database_path"`
	SourceMode      string   `json:"source_mode,omitempty"`
	SourceRoot      string   `json:"source_root,omitempty"`
	SiteSources     []string `json:"site_sources,omitempty"`
	DatabaseSources []string `json:"database_sources,omitempty"`
}

func sourceDirs(name string) []string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

func (r *receiver) pathView() pathView {
	pair := r.archivePaths("")
	root := env("BZ_BACKUP_DISPLAY_ROOT", r.backupDir)
	base := root
	if !r.split {
		base = filepath.Join(root, "YYYY-MM-DD")
	}
	return pathView{
		archivePaths: pair, Editable: r.split, BackupRoot: root,
		SitePath:     filepath.ToSlash(filepath.Join(base, pair.Site)),
		DatabasePath: filepath.ToSlash(filepath.Join(base, pair.Database)),
		SourceMode:   os.Getenv("BZ_SOURCE_MODE"), SourceRoot: os.Getenv("BZ_SOURCE_ROOT"),
		SiteSources: sourceDirs("BZ_SITE_SOURCES"), DatabaseSources: sourceDirs("BZ_DATABASE_SOURCES"),
	}
}

func (r *receiver) setPaths(w http.ResponseWriter, req *http.Request) {
	if !isJSON(req) {
		jsonReply(w, 415, result{Error: "仅支持 JSON 请求"})
		return
	}
	var input archivePaths
	err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096)).Decode(&input)
	if err != nil || !input.valid() {
		jsonReply(w, 400, result{Error: "请填写两个不同的单层目录名，可使用字母、数字、下划线和连字符"})
		return
	}
	if err := r.configurePaths(input); err != nil {
		jsonReply(w, 409, result{Error: err.Error()})
		return
	}
	jsonReply(w, 200, result{OK: true})
}

func (r *receiver) configurePaths(input archivePaths) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.split {
		return fmt.Errorf("当前部署没有开启分类存放")
	}
	for _, job := range r.runs {
		if job.Status == "transferring" || job.Status == "verifying" {
			return fmt.Errorf("有备份正在接收或校验，请完成后再修改路径")
		}
	}
	r.pathMu.Lock()
	defer r.pathMu.Unlock()
	if input == r.paths.Current {
		return nil
	}
	next := pathConfig{input, copyArchiveDays(r.paths.Days)}
	// Existing installs have no path pins until their first edit.
	for day := range r.retention.Managed {
		if _, exists := next.Days[day]; !exists {
			next.Days[day] = r.paths.Current
		}
	}
	for _, job := range r.runs {
		if _, exists := next.Days[job.Day]; !exists && r.splitPending(job.Day, job.Manifest) {
			next.Days[job.Day] = r.paths.Current
		}
	}
	for _, pair := range next.Days {
		if strings.EqualFold(input.Site, pair.Database) || strings.EqualFold(input.Database, pair.Site) {
			return fmt.Errorf("新目录与已有备份的另一分类冲突")
		}
	}
	for _, name := range []string{input.Site, input.Database} {
		path := filepath.Join(r.backupDir, name)
		if err := ensureDirectory(path, 0750); err != nil {
			return err
		}
		probe, err := os.CreateTemp(path, ".bz-rsync-write-check-")
		if err != nil {
			return fmt.Errorf("目录不可写：%s", name)
		}
		closeErr := probe.Close()
		removeErr := os.Remove(probe.Name())
		if err := errors.Join(closeErr, removeErr); err != nil {
			return err
		}
	}
	return r.savePaths(next)
}
