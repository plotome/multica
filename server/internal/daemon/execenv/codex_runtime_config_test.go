package execenv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func runtimeConfigJSON(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	doc := map[string]any{"schema": 1, "id": "fixture-runtime"}
	for k, v := range fields {
		doc[k] = v
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCodexRuntimeConfigValidation(t *testing.T) {
	for _, n := range []string{"*", "NODE_*", "node_path", "NODE_OPTIONS", "NODE_PATH", "CODEX_HOME", "MULTICA_TOKEN", "APP_SECRET", "HOME", "PATH", "DYLD_INSERT_LIBRARIES", "NODE_REPL_UNTRUSTED_ENV_ALLOWLIST", "LCU_NODE_REPL_SANDBOX", "NODE_REPL_REQUEST_META", "X_APPROVAL_MODE"} {
		if _, err := ParseCodexRuntimeConfig(runtimeConfigJSON(t, map[string]any{"include_only": []string{n}})); err == nil {
			t.Errorf("include_only accepted %q", n)
		}
	}
	for _, n := range []string{"NODE_OPTIONS", "CODEX_HOME", "LCU_NODE_REPL_SANDBOX", "NODE_REPL_REQUEST_META", "API_TOKEN", "lower"} {
		if _, err := ParseCodexRuntimeConfig(runtimeConfigJSON(t, map[string]any{"set": map[string]string{n: "x"}})); err == nil {
			t.Errorf("set accepted %q", n)
		}
	}
	for _, raw := range []string{
		``,
		`{"schema":2,"id":"v","include_only":["A"]}`,
		`{"schema":1,"id":" ","include_only":["A"]}`,
		`{"schema":1,"id":"v"}`,
		`{"schema":1,"id":"v","include_only":["A","A"]}`,
		`{"schema":1,"id":"v","include_only":["A"],"extra":true}`,
		`{"schema":1,"id":"v","include_only":["A"]} {}`,
		`{"schema":1,"id":"v","include_only":["A"],"agent_ids":[]}`,
		`{"schema":1,"id":"v","task_home_mcp_servers":["a.b"]}`,
		`{"schema":1,"id":"v","set":{"A":"x\u0000"}}`,
	} {
		if _, err := ParseCodexRuntimeConfig([]byte(raw)); err == nil {
			t.Errorf("accepted invalid %s", raw)
		}
	}
	// The untrusted worker allowlist may only be a fixed owner value.
	cfg, err := ParseCodexRuntimeConfig(runtimeConfigJSON(t, map[string]any{
		"include_only": []string{"LATE_B", "LATE_A"},
		"set":          map[string]string{"NODE_REPL_UNTRUSTED_ENV_ALLOWLIST": "A,B", "FIXED": "v"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.ShellNames(), []string{"FIXED", "LATE_A", "LATE_B", "NODE_REPL_UNTRUSTED_ENV_ALLOWLIST"}) {
		t.Fatal(cfg.ShellNames())
	}
	if !cfg.AppliesTo("any-agent") {
		t.Fatal("config without agent_ids must cover every Codex agent")
	}
	scoped, err := ParseCodexRuntimeConfig(runtimeConfigJSON(t, map[string]any{"include_only": []string{"A"}, "agent_ids": []string{"agent-1"}}))
	if err != nil || !scoped.AppliesTo("agent-1") || scoped.AppliesTo("agent-2") {
		t.Fatal("agent scope not honored", err)
	}
}

func TestCodexRuntimeConfigFileTrust(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ownership checks are unix-only")
	}
	dir := filepath.Join(t.TempDir(), "multica")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "codex-runtime.json")
	if cfg, err := LoadCodexRuntimeConfig(path); cfg != nil || err != nil {
		t.Fatalf("missing file must be disabled, got %v %v", cfg, err)
	}
	if err := os.WriteFile(path, runtimeConfigJSON(t, map[string]any{"include_only": []string{"A"}}), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := LoadCodexRuntimeConfig(path); err != nil || cfg.ID != "fixture-runtime" {
		t.Fatal(cfg, err)
	}
	os.Chmod(path, 0o620)
	if _, err := LoadCodexRuntimeConfig(path); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("group-writable file accepted: %v", err)
	}
	os.Chmod(path, 0o600)
	os.Chmod(dir, 0o777)
	if _, err := LoadCodexRuntimeConfig(path); err == nil {
		t.Fatal("world-writable directory accepted")
	}
	os.Chmod(dir, 0o755)
	link := filepath.Join(dir, "link.json")
	os.Symlink(path, link)
	if _, err := LoadCodexRuntimeConfig(link); err == nil {
		t.Fatal("symlinked config accepted")
	}
	if _, err := LoadCodexRuntimeConfig(path, filepath.Dir(dir)); err == nil || !strings.Contains(err.Error(), "outside agent workspaces") {
		t.Fatalf("config inside workspaces accepted: %v", err)
	}
	os.WriteFile(path, []byte(`{"schema":1}`), 0o600)
	if _, err := LoadCodexRuntimeConfig(path); err == nil {
		t.Fatal("invalid config must fail, not disable")
	}
}

func TestCodexRuntimeFixedValuesSurviveShellPolicy(t *testing.T) {
	cfg, err := ParseCodexRuntimeConfig(runtimeConfigJSON(t, map[string]any{
		"include_only": []string{"LATE_SERVICES"},
		"set":          map[string]string{"FIXED_BANNER": `await import("x");`},
	}))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := EnsureCodexShellEnvPolicyConfig(path, cfg.ShellNames(), cfg.Set, nil); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	policy := doc["shell_environment_policy"].(map[string]any)
	if policy["set"].(map[string]any)["FIXED_BANNER"] != `await import("x");` {
		t.Fatal(policy)
	}
	if !reflect.DeepEqual(policy["include_only"], []any{"FIXED_BANNER", "LATE_SERVICES"}) {
		t.Fatal(policy["include_only"])
	}
	// Without a runtime config the policy carries no set table.
	plain := filepath.Join(t.TempDir(), "config.toml")
	if err := EnsureCodexShellEnvPolicyConfig(plain, []string{"PATH"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(plain); strings.Contains(string(b), "set") {
		t.Fatalf("unexpected set table: %s", b)
	}
}

func TestCodexRuntimeRehomesNamedMCPServer(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	before := "[mcp_servers.other]\ncommand='other'\n[mcp_servers.other.env]\nCODEX_HOME='/owner'\n[mcp_servers.cu]\ncommand='node'\nargs=['/cu/adapter.mjs']\nenabled_tools=['js']\n[mcp_servers.cu.env]\nCODEX_HOME='/owner'\nKEEP='value'\n[mcp_servers.cu.tools.js]\napproval_mode='approve'\noutput_token_limit=25000\n[[hooks.Stop]]\nhooks=[{type='mcp_tool',server='cu',tool='turn_ended'}]\n"
	os.WriteFile(path, []byte(before), 0o644)
	if err := RehomeCodexMCPServers(path, home, []string{"cu", "absent"}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	var a, b map[string]any
	toml.Unmarshal(after, &a)
	toml.Unmarshal([]byte(before), &b)
	if !reflect.DeepEqual(a["hooks"], b["hooks"]) {
		t.Fatal("hooks changed")
	}
	servers := a["mcp_servers"].(map[string]any)
	if servers["other"].(map[string]any)["env"].(map[string]any)["CODEX_HOME"] != "/owner" {
		t.Fatal("unlisted server changed")
	}
	cu := servers["cu"].(map[string]any)
	actual, _ := filepath.EvalSymlinks(home)
	if cu["env"].(map[string]any)["CODEX_HOME"] != actual || cu["env"].(map[string]any)["KEEP"] != "value" {
		t.Fatal("task home not applied", cu["env"])
	}
	tool := cu["tools"].(map[string]any)["js"].(map[string]any)
	if _, exists := tool["approval_mode"]; exists {
		t.Fatal("owner approval leaked")
	}
	if tool["output_token_limit"] != int64(25000) {
		t.Fatal(tool)
	}
	if err := RehomeCodexMCPServers(path, home, []string{"cu"}); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != string(after) {
		t.Fatal("not idempotent")
	}
	inline := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(inline, []byte("mcp_servers = { cu = { command = 'node' } }\n"), 0o644)
	if err := RehomeCodexMCPServers(inline, filepath.Dir(inline), []string{"cu"}); err == nil {
		t.Fatal("inline registration must fail instead of leaking the owner home")
	}
}
