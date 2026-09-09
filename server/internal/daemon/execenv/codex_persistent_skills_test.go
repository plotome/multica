package execenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPersistentCodexSkillsReconcileAndPreserveNative(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	home := t.TempDir()
	native := filepath.Join(home, "skills", ".system")
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, "native"), []byte("codex-owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := []SkillContextForEnv{{Name: "one", Content: "first"}, {Name: "two", Content: "removed"}}
	if err := hydratePersistentCodexSkills(home, workspace, nil, testLogger()); err != nil {
		t.Fatal(err)
	}
	one := filepath.Join(home, "skills", "one", "SKILL.md")
	before, err := os.Stat(one)
	if err != nil {
		t.Fatal(err)
	}
	if err := hydratePersistentCodexSkills(home, workspace, nil, testLogger()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(one)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("no-op hydration mutated skill")
	}
	workspace = []SkillContextForEnv{{Name: "one", Content: "changed"}, {Name: "three", Content: "added"}}
	if err := hydratePersistentCodexSkills(home, workspace, nil, testLogger()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "skills", "two")); !os.IsNotExist(err) {
		t.Fatal("removed skill still visible")
	}
	if _, err := os.Stat(filepath.Join(home, "skills", "three", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(native, "native")); err != nil || string(b) != "codex-owned" {
		t.Fatal("native skills changed")
	}
}
