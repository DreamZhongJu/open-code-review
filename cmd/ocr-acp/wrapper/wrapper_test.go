// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package wrapper

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewRejectsEmptyBinary(t *testing.T) {
	if _, err := New("   "); err == nil || !strings.Contains(err.Error(), "--ocr") {
		t.Fatalf("want usage hint in error, got %v", err)
	}
}

func TestNewRejectsMissingAbsolutePath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-ocr")
	if _, err := New(missing); err == nil || !strings.Contains(err.Error(), "configured ocr binary not found") {
		t.Fatalf("want discovery failure, got %v", err)
	}
}

func TestNewRejectsUnknownPATHLookup(t *testing.T) {
	if _, err := New("definitely-not-ocr-binary-xyz"); err == nil ||
		!strings.Contains(err.Error(), "not found on PATH") {
		t.Fatalf("want PATH lookup failure, got %v", err)
	}
}

func TestBuildArgsDefaultsToReviewWithJSON(t *testing.T) {
	w, err := New("mock")
	if err != nil {
		t.Fatal(err)
	}
	args, err := w.buildArgs(ReviewOpts{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"review", "--format", "json", "--audience", "agent"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q (full: %v)", i, args[i], want[i], args)
		}
	}
}

func TestBuildArgsScanOmitsDiffFlagsAndKeepsRepo(t *testing.T) {
	w, _ := New("mock")
	args, err := w.buildArgs(ReviewOpts{Command: "SCAN", From: "a", To: "b", RepoArg: "/tmp/repo"})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"--from", "--to"} {
		for _, a := range args {
			if a == forbidden {
				t.Fatalf("scan must not carry %s (args=%v)", forbidden, args)
			}
		}
	}
	last := args[len(args)-2:]
	if last[0] != "--repo" || last[1] != "/tmp/repo" {
		t.Fatalf("repo override missing: %v", args)
	}
}

func collectMockRun(t *testing.T, events <-chan Event) (*OCRResult, int) {
	t.Helper()
	progress := 0
	var res *OCRResult
	for ev := range events {
		switch ev.Type {
		case EventProgress:
			progress++
		case EventResult:
			res = ev.Result
		case EventError:
			t.Fatalf("unexpected fatal event in happy path: %v", ev.Text)
		}
	}
	if res == nil {
		t.Fatalf("run produced no terminal result (%d progress events)", progress)
	}
	return res, progress
}

func TestRunReviewMockHappyPath(t *testing.T) {
	w, _ := New("mock")
	events, err := w.RunReview(context.Background(), ReviewOpts{From: "main", To: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, progress := collectMockRun(t, events)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("mock run too slow: %v", elapsed)
	}
	if progress < 3 {
		t.Fatalf("expected >=3 progress events, got %d", progress)
	}
	f := res.Findings()
	if len(f) != 2 {
		t.Fatalf("findings = %+v", f)
	}
	if f[0].Path != "internal/demo/service.go" || f[0].Severity != "high" || f[0].StartLine != 42 {
		t.Fatalf("first finding mismatched: %+v", f[0])
	}
	if res.Summary == nil || res.Summary.FilesReviewed != 7 {
		t.Fatalf("summary not decoded: %+v", res.Summary)
	}
}

func TestRunReviewMockHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, _ := New("mock")

	events, err := w.RunReview(ctx, ReviewOpts{})
	if err != nil {
		t.Fatal(err)
	}

	firstSeen := false
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("channel closed without cancel marker")
			}
			if !firstSeen && ev.Type == EventProgress {
				cancel()
				firstSeen = true
				continue
			}
			if ev.Type == EventError && strings.Contains(ev.Text, "cancelled") {
				return // success
			}
		case <-time.After(4 * time.Second):
			t.Fatalf("cancellation did not surface within 4s")
		}
	}
}

// TestParseTolerantDocument guards the lenient decoder against upstream field
// drift: unknown keys must never break finding extraction.
func TestParseTolerantDocument(t *testing.T) {
	raw := `{"status":"success","future_unknown_field":123,
	         "comments":[{"path":"x.go","content":"boom","start_line":1,"end_line":2,
			              "severity":"critical","brand_new_field":true}]}`
	var res OCRResult
	if err := tolerantUnmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Findings()) != 1 || res.Findings()[0].Severity != "critical" {
		t.Fatalf("finding lost: %+v", res)
	}
}
