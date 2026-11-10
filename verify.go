package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

var (
	digestPattern = regexp.MustCompile("^[0-9a-f]{64}$")
	archivePath   = regexp.MustCompile("^([0-9a-fA-F]{64}) [ *]((site|database)/[A-Za-z0-9_][A-Za-z0-9._-]*)$")
)

func readManifest(root *os.Root) ([]byte, error) {
	info, err := root.Lstat("SHA256SUMS")
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, fmt.Errorf("SHA256SUMS 必须为不超过 1 MiB 的普通文件")
	}
	return root.ReadFile("SHA256SUMS")
}

func verifyBatch(dir, digest string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("批次路径必须是目录")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	expected, err := manifestEntries(root, digest)
	if err != nil {
		return err
	}
	items, err := readDirectory(root, ".")
	if err != nil {
		return err
	}
	if len(items) != 3 {
		return fmt.Errorf("批次只能包含 site、database 和 SHA256SUMS")
	}
	for _, group := range []string{"site", "database"} {
		info, err := root.Lstat(group)
		if err != nil {
			return err
		}
		if !info.IsDir() || len(expected[group]) == 0 {
			return fmt.Errorf("%s 缺少清单中的备份文件", group)
		}
		items, err := readDirectory(root, group)
		if err != nil {
			return err
		}
		if len(items) != len(expected[group]) {
			return fmt.Errorf("%s 文件数量与清单不一致", group)
		}
		for _, item := range items {
			digest := expected[group][item.Name()]
			if digest == "" {
				return fmt.Errorf("%s 包含清单之外的文件 %s", group, item.Name())
			}
			if err := verifyArchive(root, group+"/"+item.Name(), digest); err != nil {
				return err
			}
		}
	}
	return nil
}

func manifestEntries(root *os.Root, digest string) (map[string]map[string]string, error) {
	files, err := readManifest(root)
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(files)) != digest {
		return nil, fmt.Errorf("清单 SHA-256 与发送端不一致")
	}
	return parseManifestEntries(files)
}

func parseManifestEntries(files []byte) (map[string]map[string]string, error) {
	expected := map[string]map[string]string{
		"site":     {},
		"database": {},
	}
	scanner := bufio.NewScanner(strings.NewReader(string(files)))
	for scanner.Scan() {
		match := archivePath.FindStringSubmatch(scanner.Text())
		if match == nil {
			return nil, fmt.Errorf("SHA256SUMS 条目格式无效")
		}
		group, name := match[3], strings.TrimPrefix(match[2], match[3]+"/")
		if _, exists := expected[group][name]; exists {
			return nil, fmt.Errorf("SHA256SUMS 包含重复条目 %s", match[2])
		}
		expected[group][name] = strings.ToLower(match[1])
	}
	return expected, scanner.Err()
}

func archiveGroupBytes(dir string, entries map[string]string) (int64, error) {
	if len(entries) == 0 {
		return 0, fmt.Errorf("归档清单缺少分类文件")
	}
	root, err := openSplitBatchDir(dir)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	var total int64
	for name := range entries {
		info, err := root.Lstat(name)
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > (1<<63-1)-total {
			return 0, fmt.Errorf("归档文件类型或大小无效：%s", name)
		}
		total += info.Size()
	}
	return total, nil
}

func manifestDigests(dir string) (rawDigest, contentDigest string, err error) {
	root, err := openSplitBatchDir(dir)
	if err != nil {
		return "", "", err
	}
	defer root.Close()
	data, err := readManifest(root)
	if err != nil {
		return "", "", err
	}
	rawDigest = fmt.Sprintf("%x", sha256.Sum256(data))
	entries, err := manifestEntries(root, rawDigest)
	if err != nil {
		return "", "", err
	}
	var lines []string
	for group, files := range entries {
		for name, digest := range files {
			lines = append(lines, fmt.Sprintf("%s  %s/%s\n", digest, group, name))
		}
	}
	sort.Strings(lines)
	return rawDigest, fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(lines, "")))), nil
}

func publishedMatches(dir, digest string) bool {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}
	defer root.Close()
	files, err := readManifest(root)
	return err == nil && fmt.Sprintf("%x", sha256.Sum256(files)) == digest
}

func readDirectory(root *os.Root, path string) ([]os.DirEntry, error) {
	file, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.ReadDir(-1)
}

func verifyArchive(root *os.Root, path, digest string) error {
	info, err := root.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("%s 必须为非空普通文件", path)
	}
	file, err := root.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return fmt.Errorf("%s SHA-256 校验失败", path)
	}
	return nil
}
