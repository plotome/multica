//go:build windows

package execenv

import (
	"errors"
	"os"
)

// Windows ACL ownership is not verified yet; refuse rather than trust the file.
func checkOwnerOnlyWritable(os.FileInfo) error {
	return errors.New("host-local Codex runtime config is not supported on Windows")
}
