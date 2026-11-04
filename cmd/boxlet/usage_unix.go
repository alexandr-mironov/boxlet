//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// dirDiskUsage суммирует фактически занятые блоки (разреженные
// raw-диски учитываются честно, не по номинальному размеру).
func dirDiskUsage(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			total += st.Blocks * 512
		} else {
			total += fi.Size()
		}
		return nil
	})
	return total, err
}
