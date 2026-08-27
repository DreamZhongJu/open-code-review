// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package wrapper

import (
	"strings"
	"testing"
)

// realisticDoc mirrors the exact emission shape of outputJSON in
// cmd/opencodereview/output.go: json.Encoder with SetIndent("", "  "), i.e. a
// multi-line indented document rather than a single compact line.
const realisticDoc = `{
  "status": "success",
  "comments": [
    {
      "path": "internal/demo/service.go",
      "content": "unchecked error return may swallow failures",
      "start_line": 42,
      "end_line": 44,
      "category": "bug",
      "severity": "high"
    },
    {
      "path": "internal/demo/api.go",
      "content": "suggestion with literal {brace} and \"nested \\\" quote\" inside string",
      "start_line": 120,
      "end_line": 126,
      "category": "maintainability",
      "severity": "medium"
    }
  ],
  "summary": {
    "files_reviewed": 7,
    "total_tokens": 12345,
    "elapsed": "1.5s"
  }
}`

func feedAll(t *testing.T, s *docState, lines ...string) (docs []string, progresses []string) {
	t.Helper()
	for _, ln := range lines {
		doc, prog := s.Feed(ln)
		if doc != "" {
			docs = append(docs, doc)
		}
		if prog != "" {
			progresses = append(progresses, prog)
		}
	}
	return docs, progresses
}

func TestDocStateAssemblesMultiLineIndentedJSON(t *testing.T) {
	lines := strings.Split(realisticDoc, "\n")
	var s docState
	docs, _ := feedAll(t, &s, lines...)
	if len(docs) != 1 {
		t.Fatalf("expected exactly one assembled document, got %d", len(docs))
	}
	var res OCRResult
	if err := tolerantUnmarshal(docs[0], &res); err != nil {
		t.Fatalf("assembled document does not parse: %v", err)
	}
	f := res.Findings()
	if len(f) != 2 {
		t.Fatalf("findings = %d, want 2", len(f))
	}
	if f[0].Path != "internal/demo/service.go" || f[0].Severity != "high" {
		t.Fatalf("first finding mismatched: %+v", f[0])
	}
	if res.Summary == nil || res.Summary.FilesReviewed != 7 {
		t.Fatalf("summary not decoded: %+v", res.Summary)
	}
}

func TestDocStateSingleLineJSONCompletesImmediately(t *testing.T) {
	var s docState
	docs, _ := feedAll(t, &s, `{"status":"success","comments":[]}`)
	if len(docs) != 1 {
		t.Fatalf("single-line doc must complete on first feed, got %d docs", len(docs))
	}
}

// Braces that live inside JSON string values (code snippets, markdown, the
// literal "} }" above) must never disturb the completion signal.
func TestDocStateIgnoresBracesInsideStrings(t *testing.T) {
	var s docState
	docs, _ := feedAll(t, &s, `{
  "content": "a } } b { c",
  "suggestion_code": "func() { return map[string]int{} }"
}`)
	if len(docs) != 1 {
		t.Fatalf("brace-in-string must not close the doc early: %d docs", len(docs))
	}
}

func TestDocStatePassesThroughProgressAndResets(t *testing.T) {
	var s docState
	// Real output may interleave progress before the document.
	docs, progs := feedAll(t, &s,
		"Preparing repository...",
		"Reviewing files...",
		"{"+`"status":"success","comments":[]`+"}",
		"All done.",
	)
	if len(progs) != 3 {
		t.Fatalf("progress lines = %d, want 3 (%v)", len(progs), progs)
	}
	if len(docs) != 1 {
		t.Fatalf("docs = %d, want 1", len(docs))
	}
}

func TestDocStateOneShotStateMachine(t *testing.T) {
	var s docState
	docs, _ := feedAll(t, &s, `{"a":1}`, `{"b":2}`)
	if len(docs) != 2 {
		t.Fatalf("sequential docs must each emit, got %d", len(docs))
	}
}

func TestDocStateDiscardsOversizedDocument(t *testing.T) {
	huge := strings.Repeat("x", maxJSONDoc+64) // single line larger than the ceiling
	var s docState
	_, progs := feedAll(t, &s, `{`, huge, `}`)
	found := false
	for _, p := range progs {
		if strings.Contains(p, "too large") {
			found = true
		}
	}
	if !found {
		t.Fatalf("oversized accumulation must be discarded with a marker, progs=%v", progs)
	}
}
