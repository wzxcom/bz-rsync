package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

type sourceRule struct {
	Directory string `json:"directory"`
	Pattern   string `json:"pattern"`
}

type SourceSelection struct {
	Revision  uint64       `json:"revision"`
	Mode      string       `json:"source_mode"`
	Websites  []sourceRule `json:"website_sources"`
	Databases []sourceRule `json:"database_sources"`
}

type sourceSettings struct {
	SourceSelection
	AppliedRevision uint64     `json:"applied_revision"`
	ApplyStatus     string     `json:"apply_status"`
	ApplyError      string     `json:"apply_error,omitempty"`
	CheckedAt       *time.Time `json:"checked_at,omitempty"`
}

const (
	defaultSitePattern = "*{date_compact}*.tar.gz"
	defaultDBPattern   = "*{date}*.sql.gz"
)

var fileGlob = regexp.MustCompile("^[A-Za-z0-9_.?*\\[\\]-]+$")

func seededRules(dirs []string, pattern string) []sourceRule {
	rules := make([]sourceRule, 0, len(dirs))
	for _, dir := range dirs {
		rules = append(rules, sourceRule{Directory: strings.TrimSpace(dir), Pattern: pattern})
	}
	return rules
}

func (r *receiver) loadSources() error {
	r.sources = sourceSettings{SourceSelection: SourceSelection{
		Mode:      "outbox",
		Websites:  seededRules(sourceDirs("BZ_SITE_SOURCES"), env("BZ_SITE_SOURCE_PATTERN", defaultSitePattern)),
		Databases: seededRules(sourceDirs("BZ_DATABASE_SOURCES"), env("BZ_DATABASE_SOURCE_PATTERN", defaultDBPattern)),
	}, ApplyStatus: "pending"}
	data, err := os.ReadFile(filepath.Join(r.stateDir, "sources.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &r.sources); err != nil {
		return err
	}
	_, err = normalizeSources(r.sources.SourceSelection)
	return err
}

func (r *receiver) storeSources(next sourceSettings) error {
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := writeAtomic(r.stateDir, "sources.json", data); err != nil {
		return err
	}
	r.sources = next
	return nil
}

func normalizeRules(rules []sourceRule, suffix, defaultPattern string) ([]sourceRule, error) {
	if len(rules) > 12 {
		return nil, fmt.Errorf("每类最多配置 12 个来源目录")
	}
	next := make([]sourceRule, 0, len(rules))
	seen := map[sourceRule]bool{}
	for _, rule := range rules {
		rule.Directory = strings.TrimSpace(rule.Directory)
		rule.Pattern = strings.TrimSpace(rule.Pattern)
		if len(rule.Directory) > 512 || rule.Directory == "/" || !path.IsAbs(rule.Directory) ||
			path.Clean(rule.Directory) != rule.Directory || strings.ContainsAny(rule.Directory, "\\\r\n\x00") {
			return nil, fmt.Errorf("宝塔目录必须是规范的 Linux 绝对路径")
		}
		if rule.Pattern == "" {
			rule.Pattern = defaultPattern
		}
		expanded := strings.NewReplacer("{date}", "2026-11-08", "{date_compact}", "20261108").Replace(rule.Pattern)
		if len(rule.Pattern) > 128 || expanded == rule.Pattern || !strings.HasSuffix(expanded, suffix) ||
			!fileGlob.MatchString(expanded) {
			return nil, fmt.Errorf("匹配规则须包含 {date} 或 {date_compact}，并以 %s 结尾", suffix)
		}
		if seen[rule] {
			return nil, fmt.Errorf("存在重复的备份来源规则")
		}
		seen[rule] = true
		next = append(next, rule)
	}
	return next, nil
}

func normalizeSources(input SourceSelection) (SourceSelection, error) {
	if input.Mode != "outbox" && input.Mode != "direct" {
		return input, fmt.Errorf("备份来源模式无效")
	}
	if input.Mode == "direct" && (len(input.Websites) == 0 || len(input.Databases) == 0) {
		return input, fmt.Errorf("直接读取模式需要分别填写网站和数据库来源")
	}
	var err error
	input.Websites, err = normalizeRules(input.Websites, ".tar.gz", defaultSitePattern)
	if err != nil {
		return input, err
	}
	input.Databases, err = normalizeRules(input.Databases, ".sql.gz", defaultDBPattern)
	return input, err
}

func (r *receiver) setSources(w http.ResponseWriter, req *http.Request) {
	if !isJSON(req) {
		jsonReply(w, 415, result{Error: "仅支持 JSON 请求"})
		return
	}
	var input SourceSelection
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 32<<10)).Decode(&input); err != nil {
		jsonReply(w, 400, result{Error: "来源设置格式无效"})
		return
	}
	input, err := normalizeSources(input)
	if err != nil {
		jsonReply(w, 400, result{Error: err.Error()})
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.sources
	if input.Mode != current.Mode || !slices.Equal(input.Websites, current.Websites) ||
		!slices.Equal(input.Databases, current.Databases) {
		input.Revision = current.Revision + 1
		next := sourceSettings{SourceSelection: input, AppliedRevision: current.AppliedRevision, ApplyStatus: "pending"}
		if err := r.storeSources(next); err != nil {
			jsonReply(w, 500, result{Error: "无法保存宝塔来源设置"})
			return
		}
	}
	jsonReply(w, 200, result{OK: true, SourceSelection: &r.sources.SourceSelection})
}

func (r *receiver) senderSources() (result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.sources.SourceSelection
	return result{OK: true, SourceSelection: &current}, nil
}

func (r *receiver) reportSources(c command) (result, error) {
	if c.ConfigOK == nil {
		return result{}, fmt.Errorf("发送端缺少配置应用结果")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	next := r.sources
	if next.Revision != c.Revision || c.Revision == 0 {
		return result{OK: true}, nil
	}
	now := time.Now()
	next.CheckedAt = &now
	if *c.ConfigOK {
		next.AppliedRevision = c.Revision
		next.ApplyStatus, next.ApplyError = "applied", ""
	} else {
		next.ApplyStatus, next.ApplyError = "failed", cleanLine(c.Error, 400)
		if next.ApplyError == "" {
			next.ApplyError = "发送端无法应用来源配置"
		}
	}
	return result{OK: true}, r.storeSources(next)
}
