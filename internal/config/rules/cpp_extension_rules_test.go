// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package rules

import "testing"

func TestResolve_CPPAlternativeExtensions(t *testing.T) {
	rule, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}

	const wantPattern = "**/*.{cpp,cc,cxx,hpp,hxx}"
	for _, path := range []string{
		"src/worker.cxx",
		"include/worker.hxx",
	} {
		t.Run(path, func(t *testing.T) {
			detail := rule.resolveDetail(path)
			if detail.Pattern != wantPattern {
				t.Fatalf("resolveDetail(%q).Pattern = %q, want %q", path, detail.Pattern, wantPattern)
			}
		})
	}
}
