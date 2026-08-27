// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package types defines the subset of the Agent Client Protocol (ACP) v1
// surface implemented by this prototype server, plus JSON-RPC 2.0 envelopes.
//
// Scope intentionally kept small for the OSPP prototype phase:
// initialize, authenticate (unsupported reply), session/new, session/prompt,
// session/cancel, and the agent -> client notifications
// available_commands_update and session/update (agent_message_chunk only).
package acp

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Protocol version constants. The prototype targets the ACP v1 baseline as
// required by the ROADMAP item and falls back gracefully for anything newer.
const (
	ACPVersionV1 = 1
)

// JSON-RPC standard error codes reused verbatim, plus one application-level
// code reserved for the HTTP transport stub below.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeNotImplemented = -32000 // e.g. streamable HTTP transport pending
)

// Request is an incoming JSON-RPC request or notification. Notifications have
// a null/absent ID and never receive a response.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// HasID reports whether the envelope carries a response ID (request rather
// than notification).
func (r *Request) HasID() bool { return len(r.ID) > 0 && string(r.ID) != "null" }

// Response is an outgoing JSON-RPC response carrying exactly one of Result or
// Error.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Notification is an outgoing JSON-RPC event without an ID.
type Notification struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// RPCError is the JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("jsonrpc %d: %s", e.Code, e.Message)
}

// ---------------------------------------------------------------------------
// initialize
// ---------------------------------------------------------------------------

// ClientCapabilities mirrors the fields of interest in ACP v1. The prototype
// never issues fs/* callbacks itself (the wrapped ocr subprocess reads the
// working tree directly), so these are recorded only for diagnostics.
type ClientCapabilities struct {
	FileSystem struct {
		ReadTextFile  bool `json:"readTextFile"`
		WriteTextFile bool `json:"writeTextFile"`
	} `json:"fs"`
	Terminal bool `json:"terminal"`
}

// InitializeParams is the client -> agent handshake payload.
type InitializeParams struct {
	RawVersion         json.RawMessage    `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
}

// ProtocolVersion parses either numeric or string encodings of the version.
func (p *InitializeParams) ProtocolVersion() (int, error) {
	s := strings.TrimSpace(string(p.RawVersion))
	if s == "" {
		return 0, fmt.Errorf("protocolVersion missing")
	}
	if s[0] == '"' {
		unquoted, err := strconv.Unquote(s)
		if err != nil {
			return 0, fmt.Errorf("invalid protocolVersion string")
		}
		s = unquoted
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("protocolVersion is not an integer")
	}
	return v, nil
}

// AgentCapabilities describes what this agent supports in the session scope.
type AgentCapabilities struct {
	LoadSession        bool                     `json:"loadSession"`
	PromptCapabilities PromptCapabilitiesSubset `json:"promptCapabilities"`
}

// PromptCapabilitiesSubset advertises fine-grained prompt features. The
// prototype relies on plain text blocks only, so most flags stay false.
type PromptCapabilitiesSubset struct {
	Audio         bool `json:"audio"`
	Image         bool `json:"image"`
	EmbeddedCtx   bool `json:"embeddedContext"`
	Mention       bool `json:"meta"`
	ResourceLinks bool `json:"resource_links"`
}

// AuthMethod describes a login scheme. An empty (but non-null) array signals
// "no authentication required", which the prototype declares explicitly.
type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// InitializeResult is the agent -> client handshake reply.
type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AuthMethods       []AuthMethod      `json:"authMethods"`
}

// ---------------------------------------------------------------------------
// session/new
// ---------------------------------------------------------------------------

// NewSessionParams carries the workspace the wrapped ocr binary will operate
// on. MCP servers forwarded by the client are accepted but unused here.
type NewSessionParams struct {
	Cwd        string          `json:"cwd"`
	ClientInfo json.RawMessage `json:"clientInfo,omitempty"`
	McpServers json.RawMessage `json:"mcpServers,omitempty"`
}

// InputHint provides UI hints for slash command arguments.
type InputHint struct {
	Hint string `json:"hint"`
}

// SlashCommand is a conversation shortcut exposed via
// available_commands_update.
type SlashCommand struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	InputHint   *InputHint `json:"inputHint,omitempty"`
}

// BuiltinCommands returns the command catalog advertised for every session.
func BuiltinCommands() []SlashCommand {
	return []SlashCommand{
		{
			Name:        "/review",
			Description: "Run an AI diff review on the current workspace (or a ref range)",
			InputHint:   &InputHint{Hint: "[--from <ref>] [--to <ref>] [--commit <sha>]"},
		},
		{
			Name:        "/scan",
			Description: "Scan a repository tree without diff selection",
			InputHint:   &InputHint{Hint: "[--repo <path>...]"},
		},
	}
}

// NewSessionResult is returned for session/new.
type NewSessionResult struct {
	SessionID string         `json:"sessionId"`
	Commands  []SlashCommand `json:"commands"`
}

// CommandsUpdateParams is the payload of available_commands_update.
type CommandsUpdateParams struct {
	SessionID string         `json:"sessionId"`
	Commands  []SlashCommand `json:"commands"`
}

// ---------------------------------------------------------------------------
// session/prompt, session/update, session/cancel
// ---------------------------------------------------------------------------

// ContentBlock accepts ACP blocks but only TextBlock bodies are consumed.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// PromptParams wraps the user turn.
type PromptParams struct {
	SessionID     string         `json:"sessionId"`
	ContentBlocks []ContentBlock `json:"content"`
}

// PromptResult terminates a prompt turn with a stop reason such as end_turn
// or cancelled.
type PromptResult struct {
	StopReason string `json:"stopReason"`
}

// Terminal stop reasons per ACP v1.
const (
	StopEndTurn   = "end_turn"
	StopCancelled = "cancelled"
	StopRefused   = "refused"
	StopMaxTokens = "max_tokens"
)

// SessionUpdateKind distinguishes the update variants inside a
// session/update notification.
type SessionUpdateKind string

// The prototype only emits streaming assistant text. Tool-call and plan
// updates remain future work tracked in PROTOTYPE.md.
const (
	UpdateAgentMessageChunk SessionUpdateKind = "agent_message_chunk"
)

// ChunkUpdate carries one streamed text delta.
type ChunkUpdate struct {
	SessionUpdate SessionUpdateKind `json:"sessionUpdate"`
	Content       ContentBlock      `json:"content"`
}

// UpdateChunkParams is an agent_message_chunk session/update payload. Chunks
// concatenate into the assistant turn in the client UI.
type UpdateChunkParams struct {
	SessionID string      `json:"sessionId"`
	Update    ChunkUpdate `json:"update"`
}

// NewTextChunk builds a text delta event for the given session.
func NewTextChunk(sessionID, text string) UpdateChunkParams {
	return UpdateChunkParams{
		SessionID: sessionID,
		Update: ChunkUpdate{
			SessionUpdate: UpdateAgentMessageChunk,
			Content:       ContentBlock{Type: "text", Text: text},
		},
	}
}

// CancelParams identifies the prompt turn to interrupt.
type CancelParams struct {
	SessionID string `json:"sessionId"`
}
