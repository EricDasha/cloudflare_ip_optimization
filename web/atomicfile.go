package main

import (
	"os"
	"path/filepath"
)

// writeFileAtomic 以「临时文件 + fsync + rename + 目录 fsync」落盘。
//
// 旧实现只做 WriteFile + Rename：rename 本身是原子的，但数据可能还停在
// 页缓存里，NAS 断电/容器被 kill 后会留下一个「名字对、内容空/截断」的
// JSON。CFData-WEB 的 atomicWriteFile 补上了 Sync，这里照做。
//
// 临时文件放在同目录，保证 rename 不跨文件系统；失败时清理临时文件。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	// rename 只保证目录项原子，目录本身也要落盘，否则断电后仍可能
	// 回到旧文件名。目录 fsync 失败不改变已完成的 rename，只忽略。
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
