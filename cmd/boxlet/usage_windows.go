package main

import (
	"os"
	"path/filepath"
)

// dirDiskUsage на Windows считает по логическим размерам файлов.
func dirDiskUsage(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		total += fi.Size()
		return nil
	})
	return total, err
}
