//go:build !windows

package main

import (
	"fmt"
	"syscall"
)

func checkDiskSpace(path string, minFreeBytes uint64) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return err
	}
	freeBytes := uint64(stat.Bavail) * uint64(stat.Bsize)
	if freeBytes < minFreeBytes {
		return fmt.Errorf("insufficient disk space: only %d MB available, need at least %d MB", freeBytes/(1024*1024), minFreeBytes/(1024*1024))
	}
	return nil
}
