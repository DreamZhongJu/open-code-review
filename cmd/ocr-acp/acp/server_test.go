// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/cmd/ocr-acp/wrapper"
)

// harness wires a Conn to an in-process client for tests.
type harness struct {
	srv    *Server
	in     io.WriteCloser // test writes envelopes here (server reads)
	out    *bufio.Reader  // test reads replies here (server writes)
	closer func()
}

func newHarness(t *testing.T, ocrBin string) *harness {
	t.Helper()
	pr1, pw1 := io.Pipe() // test -> server
	pr2, pw2 := io.Pipe() // server -> test

	w, err := wrapper.New(ocrBin)
	if err != nil {
		t.Fatalf("wrapper.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := NewServer(ctx, NewConn(pr1, pw2), w, 30*time.Second)
	go srv.Serve()

	h := &harness{srv: srv, in: pw1, out: bufio.NewReaderSize(pr2, 64*1024), closer: cancel}
	t.Cleanup(func() {
		cancel()
		pw1.Close()
	})
	return h
}

// send writes one ndjson envelope and flushes it.
func (h *harness) send(t *testing.T, raw string) {
	t.Helper()
	if _, err := fmt.Fprintln(h.in, raw); err != nil {
		t.Fatalf("send: %v", err)
	}
}

// readMsg decodes the next envelope into a generic map with a deadline so a
// regression cannot hang CI. Returns error kind when "error" key present.
func (h *harness) readMsg(t *testing.T) map[string]any {
	t.Helper()
	type box struct {
		m   map[string]any
		err error
	}
	ch := make(chan box, 1)
	go func() {
		line, err := h.out.ReadString('\n')
		if err != nil {
			ch <- box{err: err}
			return
		}
		var m map[string]any
		if jerr := json.Unmarshal([]byte(line), &m); jerr != nil {
			ch <- box{err: jerr}
			return
		}
		ch <- box{m: m}
	}()
	select {
	case b := <-ch:
		if b.err != nil {
			t.Fatalf("readMsg: %v", b.err)
		}
		return b.m
	case <-time.After(8 * time.Second):
		t.Fatalf("readMsg timed out after 8s")
		return nil
	}
}

func (h *harness) initialize(t *testing.T) {
	h.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	m := h.readMsg(t)
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize failed with %+v", m["error"])
	}
	if v, _ := res["protocolVersion"].(float64); v != float64(ACPVersionV1) {
		t.Fatalf("protocolVersion = %v, want %d", v, ACPVersionV1)
	}
	methods, _ := res["authMethods"].([]any)
	if methods == nil || len(methods) != 0 {
		t.Fatalf("authMethods must be an explicit empty array, got %v", res["authMethods"])
	}
}

func TestInitializeHandshake(t *testing.T) {
	h := newHarness(t, "mock")
	h.initialize(t)
}

func TestInitializeAcceptsStringVersion(t *testing.T) {
	h := newHarness(t, "mock")
	h.send(t, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1"}}`)
	m := h.readMsg(t)
	if _, hasErr := m["error"]; hasErr {
		t.Fatalf("string v1 should negotiate, got %v", m)
	}
}

func TestInitializeRejectsOtherVersion(t *testing.T) {
	h := newHarness(t, "mock")
	h.send(t, `{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":99}}`)
	m := h.readMsg(t)
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %v", m)
	}
	if c, _ := e["code"].(float64); c != CodeInvalidParams {
		t.Fatalf("code = %v, want %d", e["code"], CodeInvalidParams)
	}
}

func TestAuthenticateReturnsSuccessWithNoAuth(t *testing.T) {
	h := newHarness(t, "mock")
	h.initialize(t)
	h.send(t, `{"jsonrpc":"2.0","id":9,"method":"authenticate","params":{}}`)
	m := h.readMsg(t)
	if _, hasErr := m["error"]; hasErr {
		t.Fatalf("authenticate must succeed with an empty authMethods advertise, got error %v", m)
	}
	if _, hasResult := m["result"]; !hasResult {
		t.Fatalf("authenticate should carry an (empty) result object")
	}
}

func TestInitializeAdvertisesAgentInfoAndCancellation(t *testing.T) {
	h := newHarness(t, "mock")
	h.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	m := h.readMsg(t)
	res := m["result"].(map[string]any)
	ai, ok := res["agentInfo"].(map[string]any)
	if !ok {
		t.Fatalf("agentInfo missing: %v", res)
	}
	if ai["name"] != "ocr-acp" || ai["version"] == "" {
		t.Fatalf("agentInfo shape wrong: %v", ai)
	}
	caps := res["agentCapabilities"].(map[string]any)
	if rc, _ := caps["requestCancellation"].(bool); !rc {
		t.Fatalf("requestCancellation should be true: %v", caps)
	}
}

func TestUnknownMethodReturnsNotFound(t *testing.T) {
	h := newHarness(t, "mock")
	h.initialize(t)
	h.send(t, `{"jsonrpc":"2.0","id":4,"method":"nope/method","params":{}}`)
	m := h.readMsg(t)
	e := m["error"].(map[string]any)
	if c, _ := e["code"].(float64); c != CodeMethodNotFound {
		t.Fatalf("code = %v", e["code"])
	}
}

// sessionRoundtrip runs the happy path: new -> prompt -> streamed chunks ->
// terminal response with findings rendered.
func TestSessionNewPromptMockFlow(t *testing.T) {
	h := newHarness(t, "mock")

	// session/new
	const idNew = 10
	h.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/new","params":{"cwd":""}}`, idNew))
	sid := ""
	var sawCommandsUpdate bool
	deadlineChunks := 0
	for sid == "" {
		m := h.readMsg(t)
		if mID(m) == idNew {
			res := m["result"].(map[string]any)
			sid = res["sessionId"].(string)
			if _, hasCommands := res["commands"]; hasCommands {
				t.Fatalf("session/new must not carry commands (official schema); got %v", res)
			}
			continue
		}
		// Commands arrive as an available_commands_update session/update variant.
		if methodOf(m) == NotifySessionUpdate {
			up := m["params"].(map[string]any)["update"].(map[string]any)
			if k, _ := up["sessionUpdate"].(string); k == string(UpdateAvailableCommands) {
				if cmds, _ := up["availableCommands"].([]any); len(cmds) == 2 {
					sawCommandsUpdate = true
				} else {
					t.Fatalf("available_commands_update must carry two builtin commands, got %v", up)
				}
			}
		}
	}
	if !sawCommandsUpdate {
		t.Fatalf("available_commands_update session/update variant missing before response")
	}

	// prompt: plain free text defaults to /review through mock backend
	const idP = 11
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      idP,
		"method":  MethodSessionPrompt,
		"params": map[string]any{
			"sessionId": sid,
			// official schema names this field "prompt"
			"prompt": []map[string]any{{"type": "text", "text": "please review"}},
		},
	}
	raw, _ := json.Marshal(payload)
	h.send(t, string(raw))

	chunks := 0
	_ = chunks
	stop := ""
	for stop == "" && !t.Failed() {
		m := h.readMsg(t)
		switch {
		case mID(m) == idP:
			res := m["result"].(map[string]any)
			stop = res["stopReason"].(string)
		case methodOf(m) == NotifySessionUpdate:
			up := m["params"].(map[string]any)["update"].(map[string]any)
			if k, _ := up["sessionUpdate"].(string); k != string(UpdateAgentMessageChunk) {
				continue // available_commands_update variant is fine here too
			}
			text := up["content"].(map[string]any)["text"].(string)
			if strings.Contains(text, "finding") {
				deadlineChunks++
			}
		case methodOf(m) == "":
			// stray response for another id would be a protocol bug
			t.Fatalf("unexpected response envelope %v", m)
		}
	}
	if stop != StopEndTurn {
		t.Fatalf("stopReason = %q, want end_turn", stop)
	}
	if deadlineChunks == 0 {
		t.Fatalf("no chunk mentioned findings; rendering broke")
	}
}

func TestCancelMidRunReturnsCancelledStop(t *testing.T) {
	h := newHarness(t, "mock")
	h.initialize(t)

	const idN = 20
	h.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/new","params":{"cwd":""}}`, idN))
	sid := collectUntilSessionReady(t, h, idN)

	const idP = 21
	promptLine := map[string]any{
		"jsonrpc": "2.0",
		"id":      idP,
		"method":  MethodSessionPrompt,
		"params": map[string]any{
			"sessionId": sid,
			"prompt":    []map[string]any{{"type": "text", "text": "/review --from main --to dev"}},
		},
	}
	raw, _ := json.Marshal(promptLine)
	h.send(t, string(raw))

	cancelSent := false
	stop := ""
	for stop == "" && !t.Failed() {
		m := h.readMsg(t)
		switch {
		case mID(m) == idP:
			stop = m["result"].(map[string]any)["stopReason"].(string)
		case methodOf(m) == NotifySessionUpdate:
			if !cancelSent {
				// Cancel as soon as the first progress tick arrives.
				cancelLine := map[string]any{
					"jsonrpc": "2.0",
					"method":  MethodSessionCancel,
					"params":  map[string]any{"sessionId": sid},
				}
				cl, _ := json.Marshal(cancelLine)
				h.send(t, string(cl))
				cancelSent = true
			}
		}
	}
	if stop != StopCancelled {
		t.Fatalf("stopReason = %q, want cancelled", stop)
	}
}

func TestUnknownSlashCommandIsRejected(t *testing.T) {
	h := newHarness(t, "mock")
	h.initialize(t)
	const idN = 30
	h.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/new","params":{"cwd":""}}`, idN))
	sid := collectUntilSessionReady(t, h, idN)

	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      31,
		"method":  MethodSessionPrompt,
		"params": map[string]any{
			"sessionId": sid,
			"prompt":    []map[string]any{{"type": "text", "text": "/frobnicate now"}},
		},
	})
	h.send(t, string(raw))
	m := h.readMsg(t)
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected validation error for unknown slash command, got %v", m)
	}
	if s, _ := e["message"].(string); !strings.Contains(s, "/frobnicate") {
		t.Fatalf("error must name the bad command, got %q", s)
	}
}

// ---------------------------------------------------------------------------
// helpers shared by several tests
// ---------------------------------------------------------------------------

func TestRenderDefaultFillers(t *testing.T) {
	if statusOr("") != "unknown" || sevOr("") != "info" || catOr("") != "other" {
		t.Fatalf("default fillers broken")
	}
}

func mID(m map[string]any) float64 {
	v, ok := m["id"].(float64)
	if !ok {
		return -1
	}
	return v
}

func methodOf(m map[string]any) string {
	s, _ := m["method"].(string)
	return s
}

// collectUntilSessionReady consumes envelopes until the response for id idNew
// yields the freshly minted session id.
func collectUntilSessionReady(t *testing.T, h *harness, idNew int) string {
	for {
		m := h.readMsg(t)
		if mID(m) == float64(idNew) {
			res := m["result"].(map[string]any)
			sid, _ := res["sessionId"].(string)
			if sid == "" {
				t.Fatalf("empty sessionId in %+v", res)
			}
			return sid
		}
	}
}
