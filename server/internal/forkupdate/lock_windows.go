package forkupdate

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func openJournalLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{}); err != nil {
		f.Close()
		return nil, fmt.Errorf("update already in progress: %w", err)
	}
	return f, nil
}

func syncDirectory(string) error { return nil }

func lockJournal(path string) (func(), error) {
	file, err := openJournalLock(path)
	if err != nil {
		return nil, err
	}
	return func() { file.Close() }, nil
}
