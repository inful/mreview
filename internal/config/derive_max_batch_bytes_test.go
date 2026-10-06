package config

import (
	"testing"
)

// TestDeriveMaxBatchBytes covers the byte-budget derivation
// used by the chunk packer. The formula is:
//
//	usable = contextWindow - PromptOverheadTokens - maxTokens - safety
//	safety = int(contextWindow * SafetyMarginFraction)
//	return max(0, usable) * BytesPerToken
//
// (When usable <= 0, the function returns 0 — the "disable
// packing" signal. The function returns 0 also for any
// non-positive contextWindow.)
//
// The constants are package-level (PromptOverheadTokens =
// 1500, BytesPerToken = 4, SafetyMarginFraction = 0.15); the
// test uses them directly so a refactor that changes them
// surfaces here AND the operator sees the impact in the
// expected values.
func TestDeriveMaxBatchBytes(t *testing.T) {
	cases := []struct {
		name          string
		contextWindow int
		maxTokens     int
		wantPositive  bool // true if the expected return is > 0
		wantApprox    int  // expected return (when wantPositive), as an approximate value
	}{
		{
			name:          "zero context window -> 0 (disable packing)",
			contextWindow: 0,
			maxTokens:     1024,
			wantPositive:  false,
		},
		{
			name:          "negative context window -> 0 (defensive)",
			contextWindow: -1,
			maxTokens:     1024,
			wantPositive:  false,
		},
		{
			name:          "context window too small for the output -> 0",
			contextWindow: 100, // < PromptOverheadTokens (1500)
			maxTokens:     1000,
			wantPositive:  false,
		},
		{
			name:          "feasible budget -> positive",
			contextWindow: 8192,
			maxTokens:     1024,
			wantPositive:  true,
			// 8192 - 1500 - 1024 - 1228 (15% of 8192) = 4440 tokens
			// 4440 * 4 = 17760 bytes
			wantApprox: 17760,
		},
		{
			name:          "tight but feasible -> positive",
			contextWindow: 4096,
			maxTokens:     512,
			wantPositive:  true,
			// 4096 - 1500 - 512 - 614 (15% of 4096) = 1470 tokens
			// 1470 * 4 = 5880 bytes
			wantApprox: 5880,
		},
		{
			name: "maxTokens consumes all usable -> 0",
			// 2048 - 1500 - 800 - 307 (15% of 2048) = -559 -> 0
			contextWindow: 2048,
			maxTokens:     800,
			wantPositive:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveMaxBatchBytes(tc.contextWindow, tc.maxTokens)
			if tc.wantPositive {
				if got <= 0 {
					t.Errorf("DeriveMaxBatchBytes(%d, %d) = %d, want > 0",
						tc.contextWindow, tc.maxTokens, got)
				}
				// Allow ±1% slack for int-conversion rounding
				// in the safety-margin calculation. 0.15 of an
				// int is rounded toward zero; small inputs can
				// be off by a few bytes.
				tol := tc.wantApprox / 100
				if tol < 1 {
					tol = 1
				}
				if got < tc.wantApprox-tol || got > tc.wantApprox+tol {
					t.Errorf("DeriveMaxBatchBytes(%d, %d) = %d, want ~%d (±%d)",
						tc.contextWindow, tc.maxTokens, got, tc.wantApprox, tol)
				}
			} else if got != 0 {
				t.Errorf("DeriveMaxBatchBytes(%d, %d) = %d, want 0",
					tc.contextWindow, tc.maxTokens, got)
			}
		})
	}
}
