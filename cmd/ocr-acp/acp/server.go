// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/open-code-review/cmd/ocr-acp/wrapper"
)

// Server implements the ACP v1 subset over a Conn, delegating review work to
// a wrapper.Wrapper instance. One Server serves one connection.
type Server struct {
	conn   *Conn
	wrap   *wrapper.Wrapper
	root   context.Context
	cancel context.CancelFunc
	runTO  time.Duration

	mu       sync.Mutex
	sessions map[string]*sessionState
}

type sessionState struct {
	id     string
	cwd    string
	active bool
	cancel context.CancelFunc // cancels the running prompt only
}

// NewServer wires the protocol stack. The supplied context governs the whole
// process lifetime; cancelling it stops in-flight reviews.
func NewServer(ctx context.Context, conn *Conn, w *wrapper.Wrapper, runTimeout time.Duration) *Server {
	sctx, cancel := context.WithCancel(ctx)
	return &Server{
		conn:     conn,
		wrap:     w,
		root:     sctx,
		cancel:   cancel,
		runTO:    runTimeout,
		sessions: make(map[string]*sessionState),
	}
}

// Close tears down every live session.
func (s *Server) Close() { s.cancel() }

// Serve reads envelopes until the peer closes the stream.
func (s *Server) Serve() error {
	for {
		req, err := s.conn.Next()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			var perr *protocolError
			if errors.As(err, &perr) {
				_ = s.conn.SendError(nil, &RPCError{Code: CodeInvalidRequest, Message: err.Error()})
				continue
			}
			return err
		}
		if !req.HasID() {
			s.handleNotification(req)
			continue
		}
		result, rpcErr := s.dispatch(req)
		if rpcErr != nil {
			_ = s.conn.SendError(req.ID, rpcErr)
			continue
		}
		if result != nil {
			_ = s.conn.SendResponse(req.ID, result)
		}
	}
}

// protocolError wraps malformed inbound envelopes.
type protocolError struct{ msg string }

func (e *protocolError) Error() string { return e.msg }

// dispatch executes request methods synchronously except session/prompt,
// which answers asynchronously once its turn reaches a stop reason.
func (s *Server) dispatch(req *Request) (any, *RPCError) {
	switch req.Method {
	case MethodInitialize:
		return s.handleInitialize(req.Params)
	case MethodAuthenticate:
		// Official schema: with an empty authMethods array the client should
		// never call authenticate, but a plain success reply keeps the
		// handshake valid if one arrives anyway.
		return AuthenticateResult{}, nil
	case MethodSessionNew:
		return s.handleSessionNew(req.Params)
	case MethodSessionPrompt:
		rpcErr := s.handlePromptAsync(req)
		if rpcErr != nil {
			return nil, rpcErr
		}
		return nil, nil // response is delivered by the prompt goroutine
	default:
		return nil, &RPCError{Code: CodeMethodNotFound,
			Message: fmt.Sprintf("method %q not found: supported: initialize, authenticate, session/new, session/prompt; plus notification session/cancel", req.Method)}
	}
}

func (s *Server) handleNotification(req *Request) {
	if req.Method == MethodSessionCancel {
		var p CancelParams
		_ = json.Unmarshal(req.Params, &p)
		s.cancelSession(p.SessionID)
		return
	}
	// Unknown notifications are tolerated per JSON-RPC rules.
}

func (s *Server) handleInitialize(raw json.RawMessage) (any, *RPCError) {
	var p InitializeParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid initialize params: " + err.Error()}
		}
	}
	v, err := p.ProtocolVersion()
	if err != nil || v != ACPVersionV1 {
		return nil, &RPCError{Code: CodeInvalidParams,
			Message: fmt.Sprintf("unsupported protocolVersion %s: this prototype speaks ACP v1 only", strings.TrimSpace(string(p.RawVersion)))}
	}
	res := InitializeResult{
		ProtocolVersion: ACPVersionV1,
		AgentCapabilities: AgentCapabilities{
			LoadSession:         false,
			PromptCapabilities:  PromptCapabilities{},
			RequestCancellation: true,
		},
		AuthMethods: []AuthMethod{}, // explicit empty array = no auth required
		AgentInfo:   Implementation{Name: "ocr-acp", Version: "0.1.0-prototype"},
	}
	return res, nil
}

func (s *Server) handleSessionNew(raw json.RawMessage) (any, *RPCError) {
	var p NewSessionParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid session/new params: " + err.Error()}
		}
	}
	cwd, err := wrapper.ResolveCwd(p.Cwd)
	if err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
	}
	id, err := newSessionID()
	if err != nil {
		return nil, &RPCError{Code: CodeInternal, Message: "session id generation failed"}
	}
	s.mu.Lock()
	s.sessions[id] = &sessionState{id: id, cwd: cwd}
	cmds := BuiltinCommands()
	s.mu.Unlock()

	// Commands travel inside a session/update notification as the
	// available_commands_update variant; they are not part of the
	// session/new response (verified against the official schema).
	_ = s.conn.SendNotification(NotifySessionUpdate, NewCommandsUpdate(id, cmds))

	return NewSessionResult{SessionID: id}, nil
}

// handlePromptAsync validates and launches the turn, returning only setup
// errors to the dispatcher.
func (s *Server) handlePromptAsync(req *Request) *RPCError {
	var p PromptParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return &RPCError{Code: CodeInvalidParams, Message: "invalid session/prompt params: " + err.Error()}
	}
	s.mu.Lock()
	sess, ok := s.sessions[p.SessionID]
	s.mu.Unlock()
	if !ok {
		return &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("unknown sessionId %q", p.SessionID)}
	}

	text := joinedText(p.Prompt)
	intent, perr := parseIntent(text)
	if perr != nil {
		return perr
	}

	runCtx, runCancel := context.WithCancel(s.root)
	s.mu.Lock()
	if sess.active {
		s.mu.Unlock()
		runCancel()
		return &RPCError{Code: CodeInvalidParams, Message: "a prompt is already running for this session"}
	}
	sess.active = true
	sess.cancel = runCancel
	s.mu.Unlock()

	go func() {
		defer func() {
			runCancel()
			s.mu.Lock()
			sess.active = false
			sess.cancel = nil
			s.mu.Unlock()
			_ = s.conn.SendResponse(req.ID, PromptResult{StopReason: intent.stopReason})
		}()
		s.streamTurn(runCtx, sess.id, sess.cwd, &intent)
	}()
	return nil
}

type turnIntent struct {
	command    string
	from       string
	to         string
	commit     string
	repoArg    string
	stopReason string
}

// parseIntent converts free text into a wrapped CLI invocation. Slash input is
// parsed flag-by-flag; plain prose defaults to a workspace /review so clients
// without command affordances stay useful.
func parseIntent(text string) (turnIntent, *RPCError) {
	it := turnIntent{command: "review", stopReason: StopEndTurn}
	t := strings.TrimSpace(text)
	if t == "" {
		return it, nil
	}
	if !strings.HasPrefix(t, "/") {
		return it, nil
	}
	fields := strings.Fields(t)
	name := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	switch name {
	case "review":
		it.command = "review"
	case "scan":
		it.command = "scan"
	default:
		return it, &RPCError{Code: CodeInvalidParams,
			Data:    BuiltinCommands(),
			Message: fmt.Sprintf("unknown command %q: available commands: %s", fields[0], catalogNames())}
	}
	for i := 1; i < len(fields); i++ {
		val := ""
		hasVal := false
		if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "--") {
			val = fields[i+1]
			hasVal = true
		}
		switch fields[i] {
		case "--from":
			if hasVal {
				it.from = val
				i++
			}
		case "--to":
			if hasVal {
				it.to = val
				i++
			}
		case "--commit":
			if hasVal {
				it.commit = val
				i++
			}
		case "--repo":
			if hasVal {
				it.repoArg = val
				i++
			}
		default:
			// Unknown tokens are ignored for prototype tolerance.
		}
	}
	return it, nil
}

func (s *Server) cancelSession(sessionID string) {
	s.mu.Lock()
	sess, ok := s.sessions[sessionID]
	if ok && sess.cancel != nil {
		sess.cancel()
	}
	s.mu.Unlock()
}

// streamTurn forwards wrapper events as session/update chunks until the run
// terminates and records the terminal stop reason back into the intent, which
// the prompt goroutine later echoes inside its single final response.
func (s *Server) streamTurn(ctx context.Context, sessionID, cwd string, it *turnIntent) {
	events, err := s.wrap.RunReview(ctx, wrapper.ReviewOpts{
		Command: it.command,
		Cwd:     cwd,
		From:    it.from,
		To:      it.to,
		Commit:  it.commit,
		RepoArg: it.repoArg,
		Timeout: s.runTO,
	})
	if err != nil {
		_ = s.conn.SendNotification(NotifySessionUpdate, NewTextChunk(sessionID, "[error] "+err.Error()))
		it.stopReason = StopRefusal
		return
	}
	var final *wrapper.OCRResult
	for ev := range events {
		switch ev.Type {
		case wrapper.EventProgress:
			if ev.Text != "" {
				_ = s.conn.SendNotification(NotifySessionUpdate, NewTextChunk(sessionID, ev.Text+"\n"))
			}
		case wrapper.EventError:
			reason := StopEndTurn
			if strings.Contains(ev.Text, "cancelled") {
				reason = StopCancelled
				it.stopReason = reason
			}
			_ = s.conn.SendNotification(NotifySessionUpdate, NewTextChunk(sessionID, "\n[ocr] "+ev.Text+"\n"))
			if reason == StopCancelled {
				return
			}
		case wrapper.EventResult:
			final = ev.Result
		}
	}
	if final != nil {
		_ = s.conn.SendNotification(NotifySessionUpdate, NewTextChunk(sessionID, renderFindings(final)))
	} else {
		_ = s.conn.SendNotification(NotifySessionUpdate, NewTextChunk(sessionID, "(no terminal result produced)"))
	}
}

// renderFindings formats the terminal document as line-referenced Markdown
// suitable for client diff jump targets later on.
func renderFindings(res *wrapper.OCRResult) string {
	f := res.Findings()
	var b strings.Builder
	fmt.Fprintf(&b, "### OpenCodeReview report (%d finding(s), status=%s)\n", len(f), statusOr(res.Status))
	if f == nil || len(f) == 0 {
		b.WriteString("\nNo findings recorded.\n")
		if res.Message != "" {
			fmt.Fprintf(&b, "\n> note: %s\n", res.Message)
		}
		return b.String()
	}
	for i, c := range f {
		fmt.Fprintf(&b, "\n%d. **[%s/%s]** `%s` L%d-L%d\n", i+1, sevOr(c.Severity), catOr(c.Category), c.Path, c.StartLine, c.EndLine)
		body := strings.TrimSpace(c.Content)
		if body == "" {
			body = "(empty content)"
		}
		b.WriteString(body + "\n")
	}
	if res.Summary != nil {
		fmt.Fprintf(&b, "\n---\nfiles_reviewed=%d total_tokens=%d elapsed=%s\n", res.Summary.FilesReviewed, res.Summary.TotalTokens, res.Summary.Elapsed)
	}
	return b.String()
}

func statusOr(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func sevOr(s string) string {
	if s == "" {
		return "info"
	}
	return s
}

func catOr(s string) string {
	if s == "" {
		return "other"
	}
	return s
}

func joinedText(blocks []ContentBlock) string {
	var parts []string
	for _, bl := range blocks {
		if bl.Type == "text" && bl.Text != "" {
			parts = append(parts, bl.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func catalogNames() string {
	cmds := BuiltinCommands()
	names := make([]string, 0, len(cmds))
	for _, c := range cmds {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func newSessionID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
