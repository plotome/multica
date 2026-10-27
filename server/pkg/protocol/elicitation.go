package protocol

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// MCPElicitation preserves the runtime's original request for human review.
// Native application and exact browser-origin approvals use the same owner review.
type MCPElicitation struct {
	ThreadID        string          `json:"threadId"`
	TurnID          string          `json:"turnId"`
	ServerName      string          `json:"serverName"`
	Mode            string          `json:"mode"`
	Message         string          `json:"message"`
	RequestedSchema json.RawMessage `json:"requestedSchema"`
	Meta            struct {
		Kind       string `json:"codex_approval_kind"`
		Connector  string `json:"connector_id"`
		ToolName   string `json:"tool_name"`
		Origin     string `json:"origin"`
		ToolParams struct {
			App string `json:"app"`
		} `json:"tool_params"`
		Persist []string `json:"persist"`
	} `json:"_meta"`
}

func ParseMCPElicitation(raw json.RawMessage) (MCPElicitation, error) {
	var p MCPElicitation
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	if p.ThreadID == "" || p.TurnID == "" || p.ServerName == "" || p.Message == "" || p.Mode != "form" || p.Meta.Kind != "mcp_tool_call" {
		return p, fmt.Errorf("unsupported MCP elicitation")
	}
	switch p.Meta.Connector {
	case "computer-use":
		if strings.TrimSpace(p.Meta.ToolParams.App) == "" || p.Meta.Origin != "" || p.Meta.ToolName != "" {
			return p, fmt.Errorf("unsupported native application approval")
		}
		if isApplicationApprovalSurface(p.Meta.ToolParams.App) {
			return p, fmt.Errorf("computer use cannot control an application that can display agent approvals: %s", p.Meta.ToolParams.App)
		}
	case "browser-use":
		if p.Meta.ToolName != "access_browser_origin" || p.Meta.ToolParams.App != "" || !validBrowserOrigin(p.Meta.Origin) || len(p.Meta.Persist) != 0 {
			return p, fmt.Errorf("unsupported browser-origin approval")
		}
	default:
		return p, fmt.Errorf("unsupported MCP approval connector")
	}
	var schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(p.RequestedSchema, &schema); err != nil || schema.Type != "object" || schema.Properties == nil || len(schema.Properties) != 0 || len(schema.Required) != 0 {
		return p, fmt.Errorf("unsupported MCP approval schema")
	}
	return p, nil
}

func validBrowserOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" &&
		u.User == nil && u.Opaque == "" && u.Path == "" && u.RawPath == "" && u.RawQuery == "" &&
		!u.ForceQuery && u.Fragment == "" && u.Host != "" && !strings.HasSuffix(u.Host, ":") &&
		u.String() == origin && strings.ToLower(u.Host) == u.Host &&
		!(u.Scheme == "http" && u.Port() == "80") && !(u.Scheme == "https" && u.Port() == "443")
}

// SubjectKey keeps application IDs and Web origins in distinct grant namespaces.
func (p MCPElicitation) SubjectKey() string {
	if p.Meta.Connector == "browser-use" {
		return "origin:" + p.Meta.Origin
	}
	return "app:" + p.Meta.ToolParams.App
}

func (p MCPElicitation) Resource() string {
	if p.Meta.Connector == "browser-use" {
		return p.Meta.Origin
	}
	return p.Meta.ToolParams.App
}

// ElicitationResponse maps only an explicit human choice. Persistence remains
// owned by the original MCP runtime; a deferred grant delivers this choice once.
func ElicitationResponse(raw json.RawMessage, choice string) (json.RawMessage, error) {
	p, err := ParseMCPElicitation(raw)
	if err != nil {
		return nil, err
	}
	switch choice {
	case "decline", "cancel":
		return json.Marshal(map[string]any{"action": choice})
	case "once":
		return json.Marshal(map[string]any{"action": "accept", "content": map[string]any{}})
	case "session", "always":
		if p.Meta.Connector == "browser-use" {
			break
		}
		for _, scope := range p.Meta.Persist {
			if scope == choice {
				return json.Marshal(map[string]any{"action": "accept", "content": map[string]any{}, "_meta": map[string]any{"persist": choice}})
			}
		}
	}
	return nil, fmt.Errorf("approval choice is not offered by this request")
}

// These hosts can display this run's own approval controls. Parent-process
// detection alone is insufficient: the Web browser need not host the daemon.
// This is defense in depth, not an OS-wide input isolation boundary. Operators
// must use a listed approval host and keep LCU's own host guard enabled.
func isApplicationApprovalSurface(app string) bool {
	id := strings.ToLower(strings.TrimSpace(app))
	for _, prefix := range []string{"com.google.chrome", "com.microsoft.edgemac", "org.mozilla.firefox", "com.brave.browser", "com.vivaldi.vivaldi", "com.operasoftware.opera", "com.apple.safari", "company.thebrowser.browser"} {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	switch id {
	case "ai.multica.desktop", "md.obsidian", "com.anthropic.claudefordesktop", "com.anthropic.claude-code", "com.openai.codex", "com.apple.terminal", "com.googlecode.iterm2", "com.mitchellh.ghostty", "dev.warp.warp-stable", "dev.warp.warp-preview", "com.github.wez.wezterm", "org.alacritty", "net.kovidgoyal.kitty", "co.zeit.hyper", "com.microsoft.vscode", "com.todesktop.230313mzl4w4u92", "dev.zed.zed":
		return true
	}
	return false
}
