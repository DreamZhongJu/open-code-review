// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package acp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Conn implements newline-delimited JSON transport for ACP v1 stdio sessions.
// Reads are single-consumer (the serve loop); writes are serialized so prompt
// goroutines and the dispatcher can interleave safely.
type Conn struct {
	r *bufio.Reader
	w io.Writer

	wmu sync.Mutex
}

// NewConn wraps a reader and writer pair into a protocol connection.
func NewConn(r io.Reader, w io.Writer) *Conn {
	br := bufio.NewReaderSize(r, 64*1024)
	return &Conn{r: br, w: w}
}

var errProtocol = errors.New("protocol violation")

// Next blocks until one complete JSON-RPC envelope arrives. Blank lines are
// skipped defensively.
func (c *Conn) Next() (*Request, error) {
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			if strings.TrimSpace(line) == "" && errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			// A trailing line without newline may still carry a message.
			if errors.Is(err, io.EOF) {
				line += "\n"
			} else {
				return nil, err
			}
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var req Request
		if err := json.Unmarshal([]byte(trimmed), &req); err != nil {
			return nil, fmt.Errorf("%w: malformed envelope: %v", errProtocol, err)
		}
		if req.JSONRPC == "" {
			req.JSONRPC = "2.0"
		}
		if req.JSONRPC != "2.0" {
			return nil, fmt.Errorf("%w: unsupported jsonrpc field %q", errProtocol, req.JSONRPC)
		}
		if req.Method == "" {
			return nil, fmt.Errorf("%w: envelope lacks a method", errProtocol)
		}
		return &req, nil
	}
}

// SendResponse writes a success response echoing the caller's raw ID bytes.
func (c *Conn) SendResponse(id json.RawMessage, result any) error {
	resp := Response{JSONRPC: "2.0", ID: id, Result: result}
	return c.write(resp)
}

// SendError writes an error response.
func (c *Conn) SendError(id json.RawMessage, rpcErr *RPCError) error {
	resp := Response{JSONRPC: "2.0", ID: id, Error: rpcErr}
	return c.write(resp)
}

// SendNotification writes an event with no ID.
func (c *Conn) SendNotification(method string, params any) error {
	return c.write(Notification{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Conn) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal outgoing envelope: %w", err)
	}
	data = append(data, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(data)
	return err
}

// Convenience method-name constants shared by dispatcher and tests.
const (
	MethodInitialize            = "initialize"
	MethodAuthenticate          = "authenticate"
	MethodSessionNew            = "session/new"
	MethodSessionPrompt         = "session/prompt"
	MethodSessionCancel         = "session/cancel"
	NotifyCommandsUpdate        = "available_commands_update"
	NotifySessionUpdate         = "session/update"
	UpdateKindAgentMessageChunk = "agent_message_chunk"
)
