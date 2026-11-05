//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Вторая копия forward не должна стартовать, пока жива первая.
func TestLockForwardSingleInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boxlet-priv")
	if err := os.WriteFile(path, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	first, err := lockForward(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockForward(path); err == nil {
		t.Fatal("вторая блокировка взята при живой первой")
	}
	first.Close()
	again, err := lockForward(path)
	if err != nil {
		t.Fatalf("после освобождения блокировка не берётся: %v", err)
	}
	again.Close()
}

func TestWatchParentDetectsChange(t *testing.T) {
	gone := make(chan struct{})
	// Заведомо чужой ppid: watchParent должен решить, что родитель сменился.
	watchParent(os.Getppid()+1, 10*time.Millisecond, func() { close(gone) })
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("смена родителя не обнаружена")
	}
}
