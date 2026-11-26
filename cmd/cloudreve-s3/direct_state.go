package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// This lock covers recovery as well as upload: a second process must not cancel
// the first process's known upstream session. A crash leaves the lock for manual
// inspection rather than guessing from a potentially reused operating-system PID.
func directStateLock(uploadDir string) (func(), error) {
	dir, err := filepath.Abs(uploadDir)
	if err != nil || os.MkdirAll(filepath.Dir(dir), 0700) != nil {
		return nil, errors.New("cannot prepare private direct-upload state")
	}
	name := dir + ".direct-lock"
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, errors.New("direct-upload state is locked or inaccessible; inspect running uploads and any stale lock before retrying")
	}
	_, writeErr := file.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(name)
		return nil, errors.New("cannot persist direct-upload process lock")
	}
	var once sync.Once
	return func() { once.Do(func() { _ = os.Remove(name) }) }, nil
}
