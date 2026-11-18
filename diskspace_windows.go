//go:build windows

package main

import "os"

func checkDiskSpace(path string, _ uint64) error {
	_, err := os.Stat(path)
	return err
}
