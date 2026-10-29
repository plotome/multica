//go:build !windows

package execenv

import (
	"errors"
	"os"
	"syscall"
)

func checkOwnerOnlyWritable(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot read file ownership")
	}
	if int(st.Uid) != os.Getuid() {
		return errors.New("must be owned by the daemon user")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("must not be group- or world-writable")
	}
	return nil
}
