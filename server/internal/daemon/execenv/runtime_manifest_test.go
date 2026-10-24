package execenv

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRuntimeEnvManifest(t *testing.T) {
	names, err := RuntimeEnvNames(nil)
	if err != nil || len(names) != 0 {
		t.Fatal(names, err)
	}
	for _, n := range []string{"*", "NODE_*", "node_path", "NODE_OPTIONS", "CODEX_HOME", "MULTICA_TOKEN", "APP_SECRET", "HOME", "PATH", "DYLD_INSERT_LIBRARIES", "NODE_REPL_UNTRUSTED_ENV_ALLOWLIST", "SANDBOX_DISABLED"} {
		b, _ := json.Marshal(map[string]any{"schema": 1, "id": "test", "names": []string{n}})
		if _, err := RuntimeEnvNames(map[string]string{RuntimeEnvManifestKey: string(b)}); err == nil {
			t.Errorf("accepted %q", n)
		}
	}
	for _, raw := range []string{"", `{"schema":2,"id":"v","names":["A"]}`, `{"schema":1,"id":"v","names":["A","A"]}`, `{"schema":1,"id":"v","names":["A"],"extra":true}`, `{"schema":1,"id":"v","names":["A"]} {}`} {
		if _, err := RuntimeEnvNames(map[string]string{RuntimeEnvManifestKey: raw}); err == nil {
			t.Errorf("accepted invalid %s", raw)
		}
	}
	names, err = RuntimeEnvNames(map[string]string{RuntimeEnvManifestKey: `{"schema":1,"id":"lcu-v0.12.1","names":["NODE_REPL_TRUSTED_SERVICES","LCU_MAC_LIFETIME_SOCKET"]}`})
	if err != nil || !reflect.DeepEqual(names, []string{"LCU_MAC_LIFETIME_SOCKET", "NODE_REPL_TRUSTED_SERVICES"}) {
		t.Fatal(names, err)
	}
}
