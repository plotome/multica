package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"time"
)

type codexElicitation struct {
	cancel context.CancelFunc
	id     json.RawMessage
}

func (c *codexClient) handleElicitation(id, params json.RawMessage) {
	p, err := protocol.ParseMCPElicitation(params)
	if err != nil || c.onElicitation == nil || p.ThreadID != c.getThreadID() || p.TurnID != c.activeTurnID() {
		c.respond(id, map[string]any{"action": "cancel"})
		if c.onMessage != nil {
			c.onMessage(Message{Type: MessageError, Content: "MCP application approval was cancelled: a supported request, active turn and human approval connection are required."})
		}
		return
	}
	parent := c.runContext
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	key := string(id)
	c.elicitationMu.Lock()
	if c.elicitationsClosed {
		c.elicitationMu.Unlock()
		cancel()
		c.respond(id, map[string]any{"action": "cancel"})
		return
	}
	if c.elicitations == nil {
		c.elicitations = map[string]codexElicitation{}
	}
	if _, exists := c.elicitations[key]; exists {
		c.elicitationMu.Unlock()
		cancel()
		return
	}
	c.elicitations[key] = codexElicitation{cancel: cancel, id: append(json.RawMessage(nil), id...)}
	c.elicitationWG.Add(1)
	c.elicitationMu.Unlock()
	if c.onMessage != nil {
		c.onMessage(Message{Type: MessageLog, Level: "info", Content: "Checking for an owner-approved application grant."})
	}
	go func() {
		defer c.elicitationWG.Done()
		defer cancel()
		response, err := c.onElicitation(ctx, params)
		var approval *protocol.ApplicationApprovalRequired
		deferredApproval := errors.As(err, &approval)
		c.elicitationMu.Lock()
		_, pending := c.elicitations[key]
		if pending {
			if err != nil && !deferredApproval && ctx.Err() == nil && c.onMessage != nil {
				c.onMessage(Message{Type: MessageError, Content: "Application approval could not be delivered or read. The request was cancelled; check the server and device connection before retrying."})
			}
			if err != nil || ctx.Err() != nil {
				response = json.RawMessage(`{"action":"cancel"}`)
			}
			// The handler cannot manufacture a grant: validate every response against
			// the exact choices offered by the original request before forwarding it.
			if !validElicitationResponse(params, response) {
				response = json.RawMessage(`{"action":"cancel"}`)
			}
			c.respond(id, response)
			delete(c.elicitations, key)
		}
		c.elicitationMu.Unlock()
		if pending && deferredApproval && ctx.Err() == nil {
			instruction := approval.Error() + "\nComputer Use was cancelled. Do not retry, loop, or switch apps to bypass this decision. If approval_required, use the existing owner decision card and wait ledger, then end this turn. If denied or expired, report that the operation cannot proceed."
			if c.onMessage != nil {
				c.onMessage(Message{Type: MessageError, Content: approval.Error()})
			}
			steerCtx, steerCancel := context.WithTimeout(ctx, 8*time.Second)
			_, steerErr := c.request(steerCtx, "turn/steer", map[string]any{
				"threadId": p.ThreadID, "expectedTurnId": p.TurnID,
				"input": []map[string]any{{"type": "text", "text": instruction}},
			})
			steerCancel()
			if steerErr != nil && c.onMessage != nil {
				c.onMessage(Message{Type: MessageError, Content: "Failed to deliver application approval result to active turn: " + steerErr.Error()})
			}
		}
		if c.onSemanticActivity != nil {
			c.onSemanticActivity("elicitation:resolved")
		}
	}()
}

func validElicitationResponse(request, response json.RawMessage) bool {
	var r struct {
		Action  string         `json:"action"`
		Content map[string]any `json:"content"`
		Meta    struct {
			Persist string `json:"persist"`
		} `json:"_meta"`
	}
	if json.Unmarshal(response, &r) != nil {
		return false
	}
	if r.Action != "accept" && r.Action != "cancel" && r.Action != "decline" {
		return false
	}
	choice := r.Action
	if r.Action == "accept" {
		if r.Content == nil || len(r.Content) != 0 {
			return false
		}
		choice = "once"
		if r.Meta.Persist != "" {
			choice = r.Meta.Persist
		}
	}
	_, err := protocol.ElicitationResponse(request, choice)
	return err == nil
}

func (c *codexClient) hasPendingElicitation() bool {
	c.elicitationMu.Lock()
	defer c.elicitationMu.Unlock()
	return len(c.elicitations) > 0
}

func (c *codexClient) cancelElicitations(reply bool) {
	c.elicitationMu.Lock()
	defer c.elicitationMu.Unlock()
	c.elicitationsClosed = true
	for key, p := range c.elicitations {
		p.cancel()
		if reply {
			c.respond(p.id, map[string]any{"action": "cancel"})
		}
		delete(c.elicitations, key)
	}
}

func (c *codexClient) resolveElicitation(params map[string]any) {
	id, err := json.Marshal(params["requestId"])
	if err != nil {
		return
	}
	c.elicitationMu.Lock()
	defer c.elicitationMu.Unlock()
	if p, ok := c.elicitations[string(id)]; ok {
		p.cancel()
		delete(c.elicitations, string(id))
	}
}
