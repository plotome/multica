package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

const nativeApproval = `{"threadId":"t","turnId":"u","serverName":"cua_repl","mode":"form","message":"Allow Fixture?","requestedSchema":{"type":"object","properties":{}},"_meta":{"codex_approval_kind":"mcp_tool_call","connector_id":"computer-use","tool_params":{"app":"fixture"},"persist":["session","always"]}}`
const originApproval = `{"threadId":"t","turnId":"u","serverName":"cua_repl","mode":"form","message":"Allow site?","requestedSchema":{"type":"object","properties":{}},"_meta":{"codex_approval_kind":"mcp_tool_call","connector_id":"browser-use","tool_name":"access_browser_origin","origin":"https://multica.ploto.cc"}}`

func TestBrowserOriginApprovalIsExactAndOnceOnly(t *testing.T) {
	p, err := ParseMCPElicitation(json.RawMessage(originApproval))
	if err != nil || p.SubjectKey() != "origin:https://multica.ploto.cc" || p.Resource() != "https://multica.ploto.cc" {
		t.Fatalf("origin request: %+v, %v", p, err)
	}
	for _, choice := range []string{"once", "decline", "cancel"} {
		if _, err := ElicitationResponse(json.RawMessage(originApproval), choice); err != nil {
			t.Fatalf("%s: %v", choice, err)
		}
	}
	for _, choice := range []string{"session", "always"} {
		if _, err := ElicitationResponse(json.RawMessage(originApproval), choice); err == nil {
			t.Fatalf("persistent origin grant accepted: %s", choice)
		}
	}
	for _, bad := range []string{
		`https://multica.ploto.cc/path`, `https://multica.ploto.cc?x=1`,
		`https://user@multica.ploto.cc`, `https://multica.ploto.cc:443`, `https://MULTICA.ploto.cc`,
		`file:///tmp/x`, `javascript:alert(1)`,
	} {
		raw := strings.Replace(originApproval, `https://multica.ploto.cc`, bad, 1)
		if _, err := ParseMCPElicitation(json.RawMessage(raw)); err == nil {
			t.Fatalf("bad origin accepted: %s", bad)
		}
	}
	for _, bad := range []string{
		strings.Replace(originApproval, `"connector_id":"browser-use"`, `"connector_id":"computer-use"`, 1),
		strings.Replace(originApproval, `"tool_name":"access_browser_origin"`, `"tool_name":"open_tab"`, 1),
		strings.Replace(originApproval, `"origin":"https://multica.ploto.cc"`, `"origin":"https://multica.ploto.cc","persist":["always"]`, 1),
		strings.Replace(originApproval, `"origin":"https://multica.ploto.cc"`, `"origin":"https://multica.ploto.cc","tool_params":{"app":"fixture"}`, 1),
	} {
		if _, err := ParseMCPElicitation(json.RawMessage(bad)); err == nil {
			t.Fatal("unsupported request accepted:", bad)
		}
	}
}

func TestElicitationChoices(t *testing.T) {
	for _, choice := range []string{"once", "session", "always", "decline", "cancel"} {
		response, err := ElicitationResponse(json.RawMessage(nativeApproval), choice)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		json.Unmarshal(response, &v)
		if choice == "once" && v["content"] == nil {
			t.Fatal("once must carry an empty object")
		}
		if (choice == "session" || choice == "always") && v["_meta"].(map[string]any)["persist"] != choice {
			t.Fatal(string(response))
		}
	}
	for _, bad := range []string{"", "accept", "allow"} {
		if _, err := ElicitationResponse(json.RawMessage(nativeApproval), bad); err == nil {
			t.Fatal(bad)
		}
	}
	if _, err := ElicitationResponse(json.RawMessage(strings.Replace(nativeApproval, `["session","always"]`, `[]`, 1)), "always"); err == nil {
		t.Fatal("invented persistent permission")
	}
	for _, bad := range []string{`{}`, strings.Replace(nativeApproval, `"properties":{}`, `"properties":{"password":{"type":"string"}}`, 1), strings.Replace(nativeApproval, `"properties":{}`, `"properties":null`, 1), strings.Replace(nativeApproval, `"form"`, `"url"`, 1)} {
		if _, err := ParseMCPElicitation(json.RawMessage(bad)); err == nil {
			t.Fatal("accepted unsupported request")
		}
	}
}

func TestElicitationCannotControlApprovalSurfaces(t *testing.T) {
	for _, app := range []string{"ai.multica.desktop", "com.google.Chrome", "com.apple.Safari", "org.mozilla.firefox", "com.microsoft.edgemac", "md.obsidian", "com.apple.Terminal", "com.anthropic.claudefordesktop"} {
		raw := json.RawMessage(strings.Replace(nativeApproval, `"app":"fixture"`, `"app":"`+app+`"`, 1))
		if _, err := ParseMCPElicitation(raw); err == nil {
			t.Fatalf("approval host accepted: %s", app)
		}
		if _, err := ElicitationResponse(raw, "once"); err == nil {
			t.Fatalf("approval host granted: %s", app)
		}
	}
}
