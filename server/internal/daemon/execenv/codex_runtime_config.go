package execenv

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// CodexRuntimeConfigEnv overrides the host-local Codex runtime config path.
const CodexRuntimeConfigEnv = "MULTICA_CODEX_RUNTIME_CONFIG"

// CodexRuntimeConfig is the owner's host-local description of what a locally
// installed Codex runtime extension needs inside each task. Multica owns the
// mechanism; the file owns runtime-specific names and values, so a runtime
// upgrade only regenerates the file.
type CodexRuntimeConfig struct {
	ID string
	// AgentIDs limits the config to these agents; nil means every Codex agent.
	AgentIDs []string
	// IncludeOnly names are created by the runtime after the daemon snapshot
	// and must survive the task shell environment filter with their own values.
	IncludeOnly []string
	// Set values are fixed by the owner and override any runtime or agent value.
	Set map[string]string
	// TaskHomeMCPServers are copied MCP registrations whose CODEX_HOME must
	// point at the task home and whose owner tool approvals must not carry over.
	TaskHomeMCPServers []string
}

var (
	exactEnvName      = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	mcpServerNameExpr = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// DefaultCodexRuntimeConfigPath is outside every workspace and task home.
func DefaultCodexRuntimeConfigPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv(CodexRuntimeConfigEnv)); p != "" {
		return filepath.Abs(p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home for Codex runtime config: %w", err)
	}
	return filepath.Join(home, ".multica", "codex-runtime.json"), nil
}

// LoadCodexRuntimeConfig returns nil when the file does not exist. Any other
// problem is an error: an invalid or unsafe file must never degrade into
// launching tasks without the runtime it describes.
func LoadCodexRuntimeConfig(path string, protectedRoots ...string) (*CodexRuntimeConfig, error) {
	if path == "" {
		return nil, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("codex runtime config %s: %w", path, err)
	}
	if err := checkCodexRuntimeConfigLocation(path, info, protectedRoots); err != nil {
		return nil, fmt.Errorf("codex runtime config %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("codex runtime config %s: %w", path, err)
	}
	cfg, err := ParseCodexRuntimeConfig(data)
	if err != nil {
		return nil, fmt.Errorf("codex runtime config %s: %w", path, err)
	}
	return cfg, nil
}

func checkCodexRuntimeConfigLocation(path string, info os.FileInfo, protectedRoots []string) error {
	if !info.Mode().IsRegular() {
		return errors.New("must be a regular file, not a symlink")
	}
	if err := checkOwnerOnlyWritable(info); err != nil {
		return err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return err
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if err := checkOwnerOnlyWritable(dirInfo); err != nil {
		return fmt.Errorf("directory %s: %w", dir, err)
	}
	for _, root := range protectedRoots {
		if root == "" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				resolved = filepath.Clean(root)
			} else {
				return err
			}
		}
		if rel, err := filepath.Rel(resolved, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("must be outside agent workspaces (%s)", resolved)
		}
	}
	return nil
}

// ParseCodexRuntimeConfig validates the file contents.
func ParseCodexRuntimeConfig(data []byte) (*CodexRuntimeConfig, error) {
	var raw struct {
		Schema             int               `json:"schema"`
		ID                 string            `json:"id"`
		AgentIDs           *[]string         `json:"agent_ids"`
		IncludeOnly        []string          `json:"include_only"`
		Set                map[string]string `json:"set"`
		TaskHomeMCPServers []string          `json:"task_home_mcp_servers"`
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	if raw.Schema != 1 || strings.TrimSpace(raw.ID) == "" {
		return nil, errors.New("schema=1 and id are required")
	}
	if len(raw.IncludeOnly) == 0 && len(raw.Set) == 0 && len(raw.TaskHomeMCPServers) == 0 {
		return nil, errors.New("include_only, set or task_home_mcp_servers is required")
	}
	cfg := &CodexRuntimeConfig{ID: raw.ID, Set: map[string]string{}}
	if raw.AgentIDs != nil {
		if len(*raw.AgentIDs) == 0 {
			return nil, errors.New("agent_ids must be omitted or non-empty")
		}
		ids, err := uniqueStrings("agent_ids", *raw.AgentIDs, func(s string) bool { return strings.TrimSpace(s) == s && s != "" })
		if err != nil {
			return nil, err
		}
		cfg.AgentIDs = ids
	}
	names, err := uniqueStrings("include_only", raw.IncludeOnly, func(n string) bool {
		return exactEnvName.MatchString(n) && !forbiddenRuntimeEnvName(n)
	})
	if err != nil {
		return nil, err
	}
	cfg.IncludeOnly = names
	for name, value := range raw.Set {
		if !exactEnvName.MatchString(name) || forbiddenFixedEnvName(name) {
			return nil, fmt.Errorf("set: prohibited name %q", name)
		}
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("set: %s contains NUL", name)
		}
		cfg.Set[name] = value
	}
	servers, err := uniqueStrings("task_home_mcp_servers", raw.TaskHomeMCPServers, mcpServerNameExpr.MatchString)
	if err != nil {
		return nil, err
	}
	cfg.TaskHomeMCPServers = servers
	return cfg, nil
}

func uniqueStrings(field string, values []string, valid func(string) bool) ([]string, error) {
	seen := map[string]bool{}
	for _, v := range values {
		if !valid(v) {
			return nil, fmt.Errorf("%s: prohibited value %q", field, v)
		}
		if seen[v] {
			return nil, fmt.Errorf("%s: duplicate value %q", field, v)
		}
		seen[v] = true
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out, nil
}

// AppliesTo reports whether the config covers the given agent.
func (c *CodexRuntimeConfig) AppliesTo(agentID string) bool {
	if c == nil {
		return false
	}
	if c.AgentIDs == nil {
		return true
	}
	for _, id := range c.AgentIDs {
		if id == agentID {
			return true
		}
	}
	return false
}

// ShellNames returns every name the shell policy must keep. Codex applies
// `set` before `include_only`, so fixed names must be included as well.
func (c *CodexRuntimeConfig) ShellNames() []string {
	if c == nil {
		return nil
	}
	names := append([]string(nil), c.IncludeOnly...)
	for name := range c.Set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Fixed values may name the untrusted worker allowlist because the owner, not
// the runtime or the agent, decides its contents. Pass-through cannot.
func forbiddenRuntimeEnvName(n string) bool {
	return n == "NODE_REPL_UNTRUSTED_ENV_ALLOWLIST" || forbiddenFixedEnvName(n)
}

// forbiddenFixedEnvName rejects credentials, Multica/Codex control, loader and
// shell control, and anything that can change sandbox or approval behavior.
func forbiddenFixedEnvName(n string) bool {
	if codexDefaultExcludesEnvKey(n) {
		return true
	}
	for _, p := range []string{"MULTICA_", "CODEX_", "DYLD_", "LD_", "BASH_", "ZSH_"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	switch n {
	case "HOME", "PATH", "USER", "SHELL", "ENV", "NODE_OPTIONS", "NODE_PATH", "PYTHONPATH", "PYTHONHOME", "TMPDIR", "TMP", "TEMP":
		return true
	}
	return strings.Contains(n, "SANDBOX") || strings.Contains(n, "PERMISSION") || strings.Contains(n, "APPROVAL") ||
		strings.HasSuffix(n, "_REQUEST_META")
}

// RehomeCodexMCPServers rebinds the named copied MCP registrations to the task
// home and drops owner tool approvals. Every other setting is preserved; the
// result is verified by parsing both documents.
func RehomeCodexMCPServers(configPath, taskHome string, servers []string) error {
	if len(servers) == 0 {
		return nil
	}
	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read config.toml: %w", err)
	}
	home, err := filepath.EvalSymlinks(taskHome)
	if err != nil {
		return fmt.Errorf("resolve task Codex home: %w", err)
	}
	content := string(data)
	for _, name := range servers {
		if content, err = rehomeMCPServer(content, home, name); err != nil {
			return fmt.Errorf("rehome MCP server %s: %w", name, err)
		}
	}
	if content == string(data) {
		return nil
	}
	return os.WriteFile(configPath, []byte(content), 0o644)
}

func rehomeMCPServer(content, home, name string) (string, error) {
	var expected map[string]any
	if err := toml.Unmarshal([]byte(content), &expected); err != nil {
		return "", fmt.Errorf("parse config: %w", err)
	}
	servers, _ := expected["mcp_servers"].(map[string]any)
	server, _ := servers[name].(map[string]any)
	if server == nil {
		return content, nil
	}
	env, _ := server["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
		server["env"] = env
	}
	env["CODEX_HOME"] = home
	if tools, ok := server["tools"].(map[string]any); ok {
		for _, raw := range tools {
			if tool, ok := raw.(map[string]any); ok {
				delete(tool, "approval_mode")
			}
		}
	}
	table := regexp.MustCompile(`^\s*\[mcp_servers\.` + regexp.QuoteMeta(name) + `(?:\.[^\]]+)?\]\s*(?:#.*)?$`)
	var kept []string
	inside, found := false, false
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			inside = table.MatchString(line)
			found = found || inside
		}
		if !inside {
			kept = append(kept, line)
		}
	}
	if !found {
		return "", errors.New("registration requires explicit tables for task isolation")
	}
	block, err := toml.Marshal(map[string]any{"mcp_servers": map[string]any{name: server}})
	if err != nil {
		return "", err
	}
	after := strings.TrimRight(strings.Join(kept, "\n"), "\n") + "\n\n" + strings.Replace(string(block), "[mcp_servers]\n", "", 1)
	var parsed map[string]any
	if err := toml.Unmarshal([]byte(after), &parsed); err != nil {
		return "", err
	}
	if !reflect.DeepEqual(parsed, expected) {
		return "", errors.New("rehome would change unrelated configuration")
	}
	return after, nil
}
