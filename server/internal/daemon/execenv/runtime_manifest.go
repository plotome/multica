package execenv

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// RuntimeEnvManifestKey is read only from the owner's persisted agent environment.
const RuntimeEnvManifestKey = "CODEX_RUNTIME_ENV_MANIFEST"

var exactEnvName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// RuntimeEnvNames authorizes names created by a runtime after the daemon's
// environment snapshot. It never supplies values or changes sandbox policy.
func RuntimeEnvNames(custom map[string]string) ([]string, error) {
	raw, present := custom[RuntimeEnvManifestKey]
	if !present {
		return nil, nil
	}
	var m struct {
		Schema int      `json:"schema"`
		ID     string   `json:"id"`
		Names  []string `json:"names"`
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: invalid manifest: %w", RuntimeEnvManifestKey, err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("%s: trailing data", RuntimeEnvManifestKey)
	}
	if m.Schema != 1 || strings.TrimSpace(m.ID) == "" || len(m.Names) == 0 {
		return nil, fmt.Errorf("%s: schema=1, id and names are required", RuntimeEnvManifestKey)
	}
	seen := map[string]bool{}
	for _, n := range m.Names {
		if !exactEnvName.MatchString(n) || forbiddenRuntimeEnvName(n) {
			return nil, fmt.Errorf("%s: prohibited name %q", RuntimeEnvManifestKey, n)
		}
		if seen[n] {
			return nil, fmt.Errorf("%s: duplicate name %q", RuntimeEnvManifestKey, n)
		}
		seen[n] = true
	}
	sort.Strings(m.Names)
	return m.Names, nil
}

func forbiddenRuntimeEnvName(n string) bool {
	if codexDefaultExcludesEnvKey(n) {
		return true
	}
	for _, p := range []string{"MULTICA_", "CODEX_", "DYLD_", "LD_", "BASH_", "ZSH_"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	switch n {
	case "HOME", "PATH", "USER", "SHELL", "ENV", "NODE_OPTIONS", "NODE_REPL_UNTRUSTED_ENV_ALLOWLIST", "PYTHONPATH", "PYTHONHOME", "TMPDIR", "TMP", "TEMP":
		return true
	}
	return strings.Contains(n, "SANDBOX") || strings.Contains(n, "PERMISSION") || strings.Contains(n, "APPROVAL")
}
