package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// verifySplitBatch checks the public archive and its private checksum manifest.
func verifySplitBatch(siteDir, databaseDir, manifestDir, expectedDigest string) error {
	manifestRoot, err := openSplitBatchDir(manifestDir)
	if err != nil {
		return err
	}
	defer manifestRoot.Close()
	expected, err := manifestEntries(manifestRoot, expectedDigest)
	if err != nil {
		return err
	}

	if err := verifySplitGroup(siteDir, "site", expected["site"]); err != nil {
		return err
	}
	return verifySplitGroup(databaseDir, "database", expected["database"])
}

func verifySplitGroup(dir, group string, expected map[string]string) error {
	if len(expected) == 0 {
		return fmt.Errorf("%s 缺少清单中的备份文件", group)
	}
	root, err := openSplitBatchDir(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	items, err := readDirectory(root, ".")
	if err != nil {
		return err
	}
	if len(items) != len(expected) {
		return fmt.Errorf("%s 文件数量与清单不一致", group)
	}
	for _, item := range items {
		digest, exists := expected[item.Name()]
		if !exists {
			return fmt.Errorf("%s 包含清单之外的文件 %s", group, item.Name())
		}
		if err := verifyArchive(root, item.Name(), digest); err != nil {
			return fmt.Errorf("%s: %w", group, err)
		}
	}
	return nil
}

func openSplitBatchDir(path string) (*os.Root, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if resolved != path {
		return nil, fmt.Errorf("批次路径不能经过符号链接 %s", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("批次路径必须是真实目录 %s", path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	if !os.SameFile(info, opened) {
		root.Close()
		return nil, fmt.Errorf("批次路径在校验时发生变化 %s", path)
	}
	return root, nil
}
