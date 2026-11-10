package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var partialName = regexp.MustCompile("^[A-Za-z0-9_][A-Za-z0-9._-]*$")

func ensureDirectory(path string, mode os.FileMode) error {
	if err := os.Mkdir(path, mode); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s 必须是实际目录", path)
	}
	return nil
}

func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (r *receiver) layoutDate(group, day string) string {
	pair := r.archivePaths(day)
	parent := pair.Site
	if group == "database" {
		parent = pair.Database
	}
	return filepath.Join(r.backupDir, parent, strings.ReplaceAll(day, "-", ""))
}

func (r *receiver) splitMeta(day string) string {
	return filepath.Join(r.backupDir, ".bz-rsync-staging", ".published", day)
}

func (r *receiver) publishedExists(day string) (bool, error) {
	if !r.split {
		return exists(filepath.Join(r.backupDir, day))
	}
	for _, path := range []string{r.splitMeta(day), r.layoutDate("site", day), r.layoutDate("database", day)} {
		present, err := exists(path)
		if present || err != nil {
			return present, err
		}
	}
	return false, nil
}

func (r *receiver) verifyPublished(day, digest string) error {
	if !r.split {
		return verifyBatch(filepath.Join(r.backupDir, day), digest)
	}
	return verifySplitBatch(r.layoutDate("site", day), r.layoutDate("database", day), r.splitMeta(day), digest)
}

func (r *receiver) verifyExisting(day, rawDigest, contentDigest string) error {
	if !digestPattern.MatchString(contentDigest) {
		return r.verifyPublished(day, rawDigest)
	}
	meta := filepath.Join(r.backupDir, day)
	if r.split {
		meta = r.splitMeta(day)
	}
	storedRaw, storedContent, err := manifestDigests(meta)
	if err != nil {
		return err
	}
	if storedRaw != rawDigest && storedContent != contentDigest {
		return fmt.Errorf("归档文件清单与发送端不一致")
	}
	return r.verifyPublished(day, storedRaw)
}

type archiveBreakdown struct {
	SiteBytes     int64 `json:"site_bytes"`
	DatabaseBytes int64 `json:"database_bytes"`
	TotalBytes    int64 `json:"total_bytes"`
}

// archivedBreakdown reads only published manifest metadata and archive sizes.
// Callers should use it for batches that have already passed verification.
func (r *receiver) archivedBreakdown(day string) (archiveBreakdown, error) {
	date, err := time.Parse(time.DateOnly, day)
	if err != nil || date.Format(time.DateOnly) != day {
		return archiveBreakdown{}, fmt.Errorf("归档日期无效")
	}
	manifestDir := filepath.Join(r.backupDir, day)
	siteDir, databaseDir := filepath.Join(manifestDir, "site"), filepath.Join(manifestDir, "database")
	if r.split {
		manifestDir = r.splitMeta(day)
		siteDir, databaseDir = r.layoutDate("site", day), r.layoutDate("database", day)
	}
	root, err := openSplitBatchDir(manifestDir)
	if err != nil {
		return archiveBreakdown{}, err
	}
	defer root.Close()
	files, err := readManifest(root)
	if err != nil {
		return archiveBreakdown{}, err
	}
	entries, err := parseManifestEntries(files)
	if err != nil {
		return archiveBreakdown{}, err
	}
	siteBytes, err := archiveGroupBytes(siteDir, entries["site"])
	if err != nil {
		return archiveBreakdown{}, err
	}
	databaseBytes, err := archiveGroupBytes(databaseDir, entries["database"])
	if err != nil {
		return archiveBreakdown{}, err
	}
	manifestBytes := int64(len(files))
	if siteBytes > (1<<63-1)-databaseBytes-manifestBytes {
		return archiveBreakdown{}, fmt.Errorf("归档批次大小超出支持范围")
	}
	return archiveBreakdown{SiteBytes: siteBytes, DatabaseBytes: databaseBytes,
		TotalBytes: siteBytes + databaseBytes + manifestBytes}, nil
}

func (r *receiver) matchesPublished(day, digest string) bool {
	if r.split {
		return publishedMatches(r.splitMeta(day), digest)
	}
	return publishedMatches(filepath.Join(r.backupDir, day), digest)
}

func (r *receiver) publish(day, digest string) error {
	source := r.staging(day)
	if err := removeEmptyPartials(source); err != nil {
		return err
	}
	if r.split {
		return r.publishSplit(day, digest)
	}
	target := filepath.Join(r.backupDir, day)
	if err := verifyBatch(source, digest); err != nil {
		return err
	}
	return moveNew(source, target)
}

func removeEmptyPartials(source string) error {
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, parent := range []string{".", "site", "database"} {
		info, err := root.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
			continue
		}
		if err != nil {
			return err
		}
		path := filepath.Join(parent, ".bz-rsync-partial")
		info, err = root.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("rsync 续传暂存项无效：%s", path)
		}
		if err := root.Remove(path); err != nil {
			return fmt.Errorf("rsync 续传目录未清空：%w", err)
		}
	}
	return nil
}

// In-place writes affect only the hidden staging tree. Reuse a legacy rsync
// partial as the target basis when it contains more data than the existing file.
func adoptLegacyPartials(source string) error {
	root, err := openSplitBatchDir(source)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, parent := range []string{".", "site", "database"} {
		info, err := root.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() {
			return fmt.Errorf("暂存批次目录无效：%s", parent)
		}
		partial := filepath.Join(parent, ".bz-rsync-partial")
		info, err = root.Lstat(partial)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() {
			return fmt.Errorf("旧版续传目录无效：%s", partial)
		}
		entries, err := readDirectory(root, partial)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if (parent == "." && name != "SHA256SUMS") || !partialName.MatchString(name) {
				return fmt.Errorf("旧版续传目录包含非预期文件：%s", name)
			}
			staged, target := filepath.Join(partial, name), filepath.Join(parent, name)
			info, err := root.Lstat(staged)
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("旧版续传文件无效：%s", name)
			}
			targetInfo, err := root.Lstat(target)
			if err == nil && !targetInfo.Mode().IsRegular() {
				return fmt.Errorf("续传目标不是普通文件：%s", name)
			}
			switch {
			case errors.Is(err, os.ErrNotExist), err == nil && info.Size() > targetInfo.Size():
				err = root.Rename(staged, target)
			case err == nil:
				err = root.Remove(staged)
			}
			if err != nil {
				return err
			}
		}
		if err := root.Remove(partial); err != nil {
			return err
		}
	}
	return nil
}

func moveNew(source, target string) error {
	present, err := exists(target)
	if err != nil {
		return err
	}
	if present {
		return fmt.Errorf("目标已存在，保留现有数据：%s", target)
	}
	return os.Rename(source, target)
}

func (r *receiver) splitPending(day, digest string) bool {
	record, err := os.ReadFile(r.staging(day) + ".publish")
	return err == nil && string(record) == digest
}

func (r *receiver) publishSplit(day, digest string) error {
	work := r.staging(day)
	recovering := r.splitPending(day, digest)
	if !recovering {
		if err := verifyBatch(work, digest); err != nil {
			return err
		}
		if present, err := r.publishedExists(day); present || err != nil {
			return fmt.Errorf("目标日期目录已经存在或不可访问")
		}
		if err := r.pinPaths(day); err != nil {
			return err
		}
		marker := work + ".publish"
		if err := writeAtomic(filepath.Dir(marker), filepath.Base(marker), []byte(digest)); err != nil {
			return err
		}
	}
	paths, err := splitPaths(work, r.layoutDate("site", day), r.layoutDate("database", day))
	if err != nil {
		return err
	}
	if recovering {
		if err := verifySplitBatch(paths[0], paths[1], work, digest); err != nil {
			return err
		}
	}
	for i, group := range []string{"site", "database"} {
		target := r.layoutDate(group, day)
		if paths[i] != target {
			if err := moveNew(paths[i], target); err != nil {
				return err
			}
		}
	}
	return moveNew(work, r.splitMeta(day))
}

// splitPaths locates each group during a resumable publish or cleanup.
func splitPaths(work, site, database string) ([2]string, error) {
	var paths [2]string
	groups := [2]string{"site", "database"}
	for i, target := range []string{site, database} {
		group := groups[i]
		source := filepath.Join(work, group)
		inWork, err := exists(source)
		if err != nil {
			return paths, err
		}
		inTarget, err := exists(target)
		if err != nil {
			return paths, err
		}
		if inWork == inTarget {
			return paths, fmt.Errorf("%s 日期目录存在冲突或缺失", group)
		}
		paths[i] = target
		if inWork {
			paths[i] = source
		}
	}
	return paths, nil
}
