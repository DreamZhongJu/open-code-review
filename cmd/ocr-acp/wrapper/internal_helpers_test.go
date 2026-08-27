// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package wrapper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMergeOutcomeFillsDefaults(t *testing.T) {
	cases := []struct {
		name    string
		parsed  *OCRResult
		wantSt  string
		wantMsg bool
	}{
		{"nil parsed becomes failed", nil, "failed", false},
		{"empty status becomes failed", &OCRResult{}, "failed", false},
		{"success preserved", &OCRResult{Status: "success"}, "success", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeOutcome(tc.parsed, nil, "")
			if got.Status != tc.wantSt {
				t.Fatalf("status = %q, want %q", got.Status, tc.wantSt)
			}
			if tc.wantMsg && got.Message == "" {
				t.Fatalf("expected diagnostic message")
			}
			if got.Findings() == nil || len(got.Findings()) != 0 {
				t.Fatalf("Findings must be non-nil empty for no comments")
			}
		})
	}
}

func TestMergeOutcomeAttachesStderrTailOnFailure(t *testing.T) {
	res := mergeOutcome(&OCRResult{Status: ""}, nil, "boom\nline2\n")
	if !strings.Contains(res.Message, "boom") {
		t.Fatalf("stderr tail not attached: %q", res.Message)
	}
}

func TestFirstLinesTruncates(t *testing.T) {
	in := "a\nb\nc\nd\ne\nf\n"
	if got := firstLines(in, 3); got != "a\nb\nc" {
		t.Fatalf("firstLines = %q", got)
	}
	if got := firstLines("only\n", 5); got != "only" {
		t.Fatalf("short input = %q", got)
	}
}

func TestTruncate(t *testing.T) {
	long := strings.Repeat("x", 250)
	if got := truncate(long, 200); len(got) != 203 { // 200 chars + "..."
		t.Fatalf("len = %d", len(got))
	}
	if got := truncate("short", 200); got != "short" {
		t.Fatalf("short passthrough = %q", got)
	}
}

func TestSummarizeSkippedAndCounted(t *testing.T) {
	skipped := summarize(&OCRResult{Status: "skipped"}, 0)
	if !strings.Contains(skipped, "Nothing was selected") {
		t.Fatalf("skipped summary = %q", skipped)
	}
	counted := summarize(&OCRResult{Comments: []findingLite{{}, {}}}, 0)
	if !strings.Contains(counted, "2 finding(s)") {
		t.Fatalf("counted summary = %q", counted)
	}
}

func TestResolveCwdVariants(t *testing.T) {
	if _, err := ResolveCwd(""); err != nil {
		t.Fatalf("empty cwd should default to workdir: %v", err)
	}
	if _, err := ResolveCwd(filepath.Join(t.TempDir(), "missing-dir")); err == nil {
		t.Fatalf("missing cwd should error")
	}
	f := filepath.Join(t.TempDir(), "file.txt")
	writeTestFile(t, f, "x")
	if _, err := ResolveCwd(f); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file cwd should error as not-a-directory, got %v", err)
	}
	okDir := t.TempDir()
	abs, err := ResolveCwd(okDir)
	if err != nil || abs != okDir {
		t.Fatalf("abs resolution mismatch: %q err=%v", abs, err)
	}
}

// cancelWithin checks the mock backend stops quickly after context loss even
// when nobody reads the event channel anymore.
func TestMockRunnerBoundedAfterCtxLoss(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w, _ := New("mock")

	events, err := w.RunReview(ctx, ReviewOpts{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-events:
	case <-time.After(3 * time.Second):
		t.Fatalf("event channel did not close within 3s after cancellation")
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
