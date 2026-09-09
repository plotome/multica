package execenv

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// hydratePersistentCodexSkills renders the same authoritative inputs as legacy
// hydration, then reconciles by content. Codex owns .system; leave its native
// installer and version migration in control of that directory. No writes are
// made through user-skill symlinks, and no-op refreshes preserve visible paths,
// file identity and timestamps. The caller holds the conversation lease.
func hydratePersistentCodexSkills(home string, workspace []SkillContextForEnv, disabled []RuntimeSkillRefForEnv, logger *slog.Logger) error {
	stage, err := os.MkdirTemp(home, ".skills-prepare-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := seedUserCodexSkills(stage, workspace, logger); err != nil {
		return err
	}
	wanted := filepath.Join(stage, "skills")
	if err := writeSkillFiles(wanted, workspace, nil); err != nil {
		return err
	}
	actual, err := privateCodexChild(home, "skills")
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(wanted)
	if err != nil {
		return err
	}
	keep := map[string]bool{".system": true}
	for _, entry := range entries {
		name := entry.Name()
		if name == ".system" {
			return fmt.Errorf("workspace skill cannot own Codex .system")
		}
		keep[name] = true
		src, dst := filepath.Join(wanted, name), filepath.Join(actual, name)
		equal, err := equalCodexSkillEntry(src, dst)
		if err != nil {
			return err
		}
		if equal {
			continue
		}
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	previous, err := os.ReadDir(actual)
	if err != nil {
		return err
	}
	for _, entry := range previous {
		if !keep[entry.Name()] {
			if err := os.RemoveAll(filepath.Join(actual, entry.Name())); err != nil {
				return err
			}
		}
	}
	return ensureCodexDisabledSkillsConfig(filepath.Join(home, "config.toml"), home, disabled, workspace)
}

func equalCodexSkillEntry(a, b string) (bool, error) {
	ai, err := os.Lstat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Lstat(b)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if ai.Mode() != bi.Mode() {
		return false, nil
	}
	if ai.Mode()&os.ModeSymlink != 0 {
		at, err := os.Readlink(a)
		if err != nil {
			return false, err
		}
		bt, err := os.Readlink(b)
		return at == bt, err
	}
	if ai.IsDir() {
		as, err := os.ReadDir(a)
		if err != nil {
			return false, err
		}
		bs, err := os.ReadDir(b)
		if err != nil {
			return false, err
		}
		if len(as) != len(bs) {
			return false, nil
		}
		for i := range as {
			if as[i].Name() != bs[i].Name() {
				return false, nil
			}
			equal, err := equalCodexSkillEntry(filepath.Join(a, as[i].Name()), filepath.Join(b, bs[i].Name()))
			if err != nil || !equal {
				return false, err
			}
		}
		return true, nil
	}
	if !ai.Mode().IsRegular() {
		return false, fmt.Errorf("unsupported skill file: %s", a)
	}
	if ai.Size() != bi.Size() {
		return false, nil
	}
	ab, err := os.ReadFile(a)
	if err != nil {
		return false, err
	}
	bb, err := os.ReadFile(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ab, bb), nil
}
