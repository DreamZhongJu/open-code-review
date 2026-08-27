// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package types defines the subset of the Agent Client Protocol (ACP) v1
// surface implemented by this prototype server, plus JSON-RPC 2.0 envelopes.
//
// Field names, enum values, and shapes were verified against the official
// ACP v1 schema (agentclientprotocol/agent-client-protocol, agent.rs and
// client.rs), not inferred: stop reasons are refusal (not refused), prompt
// carries prompt blocks (not content), and commands travel inside
// session/update as the available_commands_update variant rather than in a
// dedicated notification or in the session/new response.
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

// Implementation identifies an ACP client or agent (name/version pair), used
// in initialize.client_info and initialize.agent_info.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

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
	ClientInfo         *Implementation    `json:"clientInfo,omitempty"`
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

// PromptCapabilities advertises which ContentBlock shapes the agent accepts.
// This server only consumes text blocks, so every capability stays false.
type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// AgentCapabilities describes what this agent supports in the session scope.
type AgentCapabilities struct {
	LoadSession         bool               `json:"loadSession"`
	PromptCapabilities  PromptCapabilities `json:"promptCapabilities"`
	RequestCancellation bool               `json:"requestCancellation"`
}

// AuthMethod describes a login scheme. An empty (but non-null) array signals
// "no authentication required", which the prototype declares explicitly.
type AuthMethod struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// InitializeResult is the agent -> client handshake reply.
type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AuthMethods       []AuthMethod      `json:"authMethods"`
	AgentInfo         Implementation    `json:"agentInfo"`
}

// AuthenticateResult is the authenticate reply. With an empty authMethods
// array the client should never send authenticate, but when it does the
// protocol still expects a plain success envelope rather than an error.
type AuthenticateResult struct{}

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

// UnstructuredCommandInput accepts the free text typed after a slash command.
type UnstructuredCommandInput struct {
	Hint string `json:"hint"`
}

// AvailableCommand is a conversation shortcut advertised through the
// available_commands_update session/update variant.
type AvailableCommand struct {
	Name        string                    `json:"name"`
	Description string                    `json:"description"`
	Input       *UnstructuredCommandInput `json:"input,omitempty"`
}

// BuiltinCommands returns the command catalog advertised for every session.
func BuiltinCommands() []AvailableCommand {
	return []AvailableCommand{
		{
			Name:        "/review",
			Description: "Run an AI diff review on the current workspace (or a ref range)",
			Input:       &UnstructuredCommandInput{Hint: "[--from <ref>] [--to <ref>] [--commit <sha>]"},
		},
		{
			Name:        "/scan",
			Description: "Scan a repository tree without diff selection",
			Input:       &UnstructuredCommandInput{Hint: "[--repo <path>...]"},
		},
	}
}

// NewSessionResult is returned for session/new. Commands are NOT part of the
// official response (schema: session_id, modes, config_options); the catalog
// is pushed separately through the available_commands_update update.
type NewSessionResult struct {
	SessionID string `json:"sessionId"`
}

// ---------------------------------------------------------------------------
// session/prompt, session/update, session/cancel
// ---------------------------------------------------------------------------

// ContentBlock accepts ACP blocks but only TextBlock bodies are consumed.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// PromptParams wraps the user turn. The official schema names the field
// "prompt" (a vector of ContentBlock), not "content".
type PromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// PromptResult terminates a prompt turn with a stop reason such as end_turn
// or cancelled.
type PromptResult struct {
	StopReason string `json:"stopReason"`
}

// Terminal stop reasons per ACP v1 (agent.rs StopReason, snake_case).
const (
	StopEndTurn    = "end_turn"
	StopMaxTokens  = "max_tokens"
	StopMaxTurnReq = "max_turn_requests"
	StopRefusal    = "refusal" // spec spells it refusal, not refused
	StopCancelled  = "cancelled"
)

// SessionUpdateKind distinguishes the update variants inside a
// session/update notification.
type SessionUpdateKind string

// The prototype emits streaming assistant text and the command catalog.
const (
	UpdateAgentMessageChunk SessionUpdateKind = "agent_message_chunk"
	UpdateAvailableCommands SessionUpdateKind = "available_commands_update"
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

// CommandsUpdate is the available_commands_update variant payload.
type CommandsUpdate struct {
	SessionUpdate     SessionUpdateKind  `json:"sessionUpdate"`
	AvailableCommands []AvailableCommand `json:"availableCommands"`
}

// UpdateCommandsParams is a session/update notification advertising the
// slash command catalog, matching the official AvailableCommandsUpdate shape.
type UpdateCommandsParams struct {
	SessionID string         `json:"sessionId"`
	Update    CommandsUpdate `json:"update"`
}

// NewCommandsUpdate builds the available_commands_update event.
func NewCommandsUpdate(sessionID string, cmds []AvailableCommand) UpdateCommandsParams {
	return UpdateCommandsParams{
		SessionID: sessionID,
		Update: CommandsUpdate{
			SessionUpdate:     UpdateAvailableCommands,
			AvailableCommands: cmds,
		},
	}
}

// CancelParams identifies the prompt turn to interrupt (session/cancel
// notification, schema CancelNotification).
type CancelParams struct {
	SessionID string `json:"sessionId"`
}
