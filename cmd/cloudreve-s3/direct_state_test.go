package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirectStateLockPreventsConcurrentRecoveryAndPreservesStaleLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uploads")
	unlock, err := directStateLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if other, err := directStateLock(dir); err == nil {
		other()
		t.Fatal("second process could recover the active process's upload")
	}
	unlock()
	unlock() // A repeated release must not remove another process's new lock.
	other, err := directStateLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if _, err := os.Stat(dir + ".direct-lock"); err != nil {
		t.Fatal("old release removed another upload's lock", err)
	}
	other()
	if err := os.WriteFile(dir+".direct-lock", []byte("old-process\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if release, err := directStateLock(dir); err == nil {
		release()
		t.Fatal("stale lock was silently replaced")
	}
}
