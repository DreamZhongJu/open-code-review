// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package wrapper launches and supervises the `ocr` CLI as a child process,
// keeping all protocol handling outside open-code-review's internal packages
// as required by the ROADMAP ACP item.
//
// Two backends are supported:
//
//   - real: executes a discovered ocr binary with --format json --audience agent
//     and parses the single JSON document printed on stdout;
//   - mock: an in-process generator emitting deterministic output, used for
//     tests, demos, and development without any LLM credentials.
package wrapper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// findingLite mirrors the subset of model.LlmComment consumed by the server.
type findingLite struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Category  string `json:"category,omitempty"`
	Severity  string `json:"severity,omitempty"`
}

// summaryLite mirrors the token/coverage tallies of jsonOutput.summary.
type summaryLite struct {
	FilesReviewed int    `json:"files_reviewed"`
	TotalTokens   int64  `json:"total_tokens"`
	Elapsed       string `json:"elapsed"`
	Budget        bool   `json:"budget_exceeded"`
}

// OCRResult is the tolerant view of the emitted jsonOutput document. Unknown
// or changed fields are deliberately ignored so that protocol stability does
// not depend on parser strictness.
type OCRResult struct {
	Status   string        `json:"status"`
	Message  string        `json:"message"`
	Comments []findingLite `json:"comments"`
	Summary  *summaryLite  `json:"summary"`
}

// Findings returns the parsed comments list, never nil.
func (r *OCRResult) Findings() []findingLite {
	if r == nil || r.Comments == nil {
		return []findingLite{}
	}
	return r.Comments
}

// EventType classifies streamed wrapper events.
type EventType int

const (
	EventProgress EventType = iota // interim update for session/update
	EventResult                    // terminal parsed outcome
	EventError                     // fatal execution failure
)

// Event is one step of a review run.
type Event struct {
	Type   EventType  // progress, terminal result, or fatal error
	Text   string     // human readable chunk text
	Result *OCRResult // set on EventResult
}

// ReviewOpts selects what the wrapped binary should do.
type ReviewOpts struct {
	Command string // "review" or "scan"
	Cwd     string
	From    string
	To      string
	Commit  string
	RepoArg string
	Timeout time.Duration
}

// Wrapper supervises ocr invocations for one ACP server instance.
type Wrapper struct {
	binary string
	mock   bool
}

// New creates a wrapper. A binary value of "mock" selects the built-in demo
// backend; anything else goes through discovery.
func New(binary string) (*Wrapper, error) {
	if strings.TrimSpace(binary) == "" {
		return nil, errors.New("ocr binary path is empty: pass --ocr <path> or --ocr mock")
	}
	w := &Wrapper{binary: binary}
	w.mock = strings.EqualFold(strings.TrimSpace(binary), "mock")
	if !w.mock {
		if err := w.discover(); err != nil {
			return nil, err
		}
	}
	return w, nil
}

// discover resolves relative paths against PATH exactly once, so failures
// surface at startup instead of on the first user prompt.
func (w *Wrapper) discover() error {
	if strings.ContainsAny(w.binary, "/\\") {
		if _, err := os.Stat(w.binary); err != nil {
			return fmt.Errorf("configured ocr binary not found: %s", w.binary)
		}
		return nil
	}
	found, err := exec.LookPath(w.binary)
	if err != nil {
		return fmt.Errorf("ocr binary %q not found on PATH: use --ocr /abs/path or --ocr mock", w.binary)
	}
	w.binary = found
	return nil
}

// RunReview starts an asynchronous run and returns a stream of events closed
// when the run terminates. Context cancellation kills the child immediately.
//
// Ownership of ctx passes to the spawned goroutines: they are responsible for
// invoking cancel when the stream closes. This function itself never blocks
// and never cancels on return.
func (w *Wrapper) RunReview(parent context.Context, opts ReviewOpts) (<-chan Event, error) {
	baseCtx, baseCancel := context.WithCancel(parent)
	ctx, cancel := baseCtx, baseCancel
	if opts.Timeout > 0 {
		var timeoutCancel context.CancelFunc
		ctx, timeoutCancel = context.WithTimeout(baseCtx, opts.Timeout)
		cancel = func() { timeoutCancel(); baseCancel() }
	}
	out := make(chan Event, 16)

	if w.mock {
		go w.runMock(ctx, cancel, opts, out)
		return out, nil
	}

	args, err := w.buildArgs(opts)
	if err != nil {
		cancel()
		close(out)
		return out, err
	}
	cmd := exec.CommandContext(ctx, w.binary, args...)
	cmd.Dir = opts.Cwd
	setProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		close(out)
		return out, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		close(out)
		return out, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		close(out)
		return out, fmt.Errorf("start ocr: %w", err)
	}

	var tail stderrTail
	go tail.drain(stderr)

	started := time.Now()

	scannerDone := make(chan struct{})
	var lastParsed *OCRResult

	go func() {
		defer close(scannerDone)
		scanner := bufio.NewScanner(bufio.NewReaderSize(stdout, 1<<20))
		scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if !strings.HasPrefix(line, "{") {
				out <- Event{Type: EventProgress, Text: truncate(line, 200)}
				continue
			}
			var res OCRResult
			if err := json.Unmarshal([]byte(line), &res); err == nil {
				lastParsed = &res
			} else {
				out <- Event{Type: EventProgress, Text: truncate(line, 200)}
			}
		}
	}()

	go func() {
		defer close(out)
		defer cancel()

		heartbeat := time.NewTicker(5 * time.Second)
		defer heartbeat.Stop()

	waitLoop:
		for {
			select {
			case <-scannerDone:
				break waitLoop
			case <-ctx.Done():
				killTree(cmd.Process)
				<-scannerDone
				if ctx.Err() == context.Canceled {
					out <- Event{Type: EventError, Text: "review cancelled by client"}
				} else {
					out <- Event{Type: EventError, Text: "review timed out"}
				}
				return
			case <-heartbeat.C:
				out <- Event{Type: EventProgress,
					Text: fmt.Sprintf("ocr still running (elapsed %s)...", time.Since(started).Round(time.Second))}
			}
		}
		_ = cmd.Wait()

		res := mergeOutcome(lastParsed, cmd.ProcessState, tail.snapshot())
		out <- Event{Type: EventProgress, Text: summarize(res, time.Since(started))}
		out <- Event{Type: EventResult, Result: res}
	}()

	return out, nil
}

// runMock implements the deterministic demo backend. It streams incremental
// progress, honors cancellation within 500ms ticks, then prints a fixed
// jsonOutput-shaped document identical in shape to the real binary's result.
func (w *Wrapper) runMock(ctx context.Context, cancel context.CancelFunc, opts ReviewOpts, out chan<- Event) {
	defer close(out)
	defer cancel()

	started := time.Now()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	i := 0
	for i < 3 {
		select {
		case <-ctx.Done():
			out <- Event{Type: EventError, Text: "review cancelled by client"}
			return
		case <-tick.C:
			i++
			out <- Event{Type: EventProgress,
				Text: fmt.Sprintf("mock review in progress (%d/3 steps)", i)}
		}
	}

	scope := "workspace diff"
	if opts.From != "" && opts.To != "" {
		scope = fmt.Sprintf("%s..%s", opts.From, opts.To)
	} else if opts.Commit != "" {
		scope = "commit " + opts.Commit
	}
	mockDoc := map[string]any{
		"status": "success",
		"comments": []map[string]any{
			{
				"path":       "internal/demo/service.go",
				"content":    "Mock finding: unchecked error return may swallow failures.",
				"start_line": 42,
				"end_line":   44,
				"category":   "bug",
				"severity":   "high",
			},
			{
				"path":       "internal/demo/api.go",
				"content":    "Mock finding: response body is never closed, leaking connections.",
				"start_line": 120,
				"end_line":   126,
				"category":   "maintainability",
				"severity":   "medium",
			},
		},
		"summary": map[string]any{
			"files_reviewed": 7,
			"total_tokens":   12345,
			"elapsed":        time.Since(started).Round(time.Second).String(),
		},
		"mock_scope": scope,
	}
	raw, _ := json.Marshal(mockDoc)
	var res OCRResult
	_ = json.Unmarshal(raw, &res)
	out <- Event{Type: EventProgress, Text: summarize(&res, time.Since(started))}
	out <- Event{Type: EventResult, Result: &res}
}

// buildArgs assembles the argv for the wrapped CLI. The JSON flag pair matches
// addOutputFlags in cmd/opencodereview/shared_flags.go; the repo override lets
// the client operate on any directory reachable by the server host.
func (w *Wrapper) buildArgs(opts ReviewOpts) ([]string, error) {
	sub := strings.ToLower(strings.TrimSpace(opts.Command))
	switch sub {
	case "review":
	case "scan":
	default:
		sub = "review" // free-text prompts without a slash command default to review
	}
	args := []string{sub, "--format", "json", "--audience", "agent"}
	if sub == "review" {
		if opts.From != "" {
			args = append(args, "--from", opts.From)
		}
		if opts.To != "" {
			args = append(args, "--to", opts.To)
		}
		if opts.Commit != "" {
			args = append(args, "--commit", opts.Commit)
		}
	}
	if opts.RepoArg != "" {
		args = append(args, "--repo", opts.RepoArg)
	}
	return args, nil
}

// stderrTail keeps a bounded copy of everything the child wrote to stderr so a
// failed run can still surface diagnostics without polluting stdout.
type stderrTail struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (t *stderrTail) drain(r io.Reader) {
	s := bufio.NewScanner(io.LimitReader(r, 8<<20))
	for s.Scan() {
		t.mu.Lock()
		if t.data.Len() > 4096 {
			t.data.Reset()
		}
		t.data.WriteString(s.Text())
		t.data.WriteByte('\n')
		t.mu.Unlock()
	}
}

func (t *stderrTail) snapshot() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.data.String()
}

// mergeOutcome reconciles the parsed document, the exit state, and stderr
// into one terminal OCRResult.
//
// Rules:
//   - missing parsed document or blank status resolves conservatively;
//     an absent process state (cancellation paths) maps to failed;
//   - diagnostic tails are attached only when the run produced neither
//     findings nor usage tallies, so healthy outputs stay untouched.
func mergeOutcome(parsed *OCRResult, ps *os.ProcessState, stderrTailText string) *OCRResult {
	res := parsed
	if res == nil {
		res = &OCRResult{}
	}
	switch {
	case res.Status == "" && ps == nil:
		res.Status = "failed"
	case res.Status == "" && ps != nil && ps.Success():
		res.Status = "success"
	case res.Status == "":
		res.Status = "failed"
	}

	failedExit := ps != nil && !ps.Success()
	hasTail := strings.TrimSpace(stderrTailText) != ""
	if (failedExit || hasTail) && res.Comments == nil && res.Summary == nil && res.Message == "" {
		var head string
		if failedExit {
			head = "ocr exited nonzero. "
		} else {
			head = "(stderr) "
		}
		res.Message = strings.TrimSpace(head + firstLines(stderrTailText, 6))
	}
	return res
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func summarize(res *OCRResult, elapsed time.Duration) string {
	n := len(res.Findings())
	head := fmt.Sprintf("OpenCodeReview finished in %s with %d finding(s).", elapsed.Round(time.Second), n)
	if res.Status == "skipped" {
		return head + " Nothing was selected for review."
	}
	return head
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ResolveCwd normalizes the workspace root supplied by clients.
func ResolveCwd(cwd string) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		return os.Getwd()
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve cwd %q: %w", cwd, err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("session cwd %q: %w", abs, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("session cwd %q is not a directory", abs)
	}
	return abs, nil
}

// tolerantUnmarshal decodes an ocr stdout document. It exists so tests can pin
// the leniency contract: upstream jsonOutput may gain fields at any time
// without breaking this adapter.
func tolerantUnmarshal(raw string, dst *OCRResult) error {
	return json.Unmarshal([]byte(raw), dst)
}
