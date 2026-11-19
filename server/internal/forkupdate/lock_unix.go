//go:build !windows

package forkupdate

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openJournalLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("update already in progress: %w", err)
	}
	return f, nil
}

func syncDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func lockJournal(path string) (func(), error) {
	file, err := openJournalLock(path)
	if err != nil {
		return nil, err
	}
	return func() { file.Close() }, nil
}
