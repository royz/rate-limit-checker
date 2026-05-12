package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ─── parseRateLimitHeaders ────────────────────────────────────────────────────

func makeResp(headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{Header: h}
}

func TestParseRateLimitHeaders_XRateLimitVariant(t *testing.T) {
	resp := makeResp(map[string]string{
		"X-RateLimit-Limit":     "100",
		"X-RateLimit-Remaining": "42",
		"X-RateLimit-Window":    "60",
	})
	info := parseRateLimitHeaders(resp)

	if info.Limit != 100 {
		t.Errorf("Limit: got %d, want 100", info.Limit)
	}
	if info.Remaining != 42 {
		t.Errorf("Remaining: got %d, want 42", info.Remaining)
	}
	if info.WindowSecs != 60 {
		t.Errorf("WindowSecs: got %d, want 60", info.WindowSecs)
	}
}

func TestParseRateLimitHeaders_RateLimitVariant(t *testing.T) {
	// IETF draft headers: RateLimit-Limit, RateLimit-Remaining
	resp := makeResp(map[string]string{
		"RateLimit-Limit":     "200",
		"RateLimit-Remaining": "150",
		"RateLimit-Window":    "30",
	})
	info := parseRateLimitHeaders(resp)

	if info.Limit != 200 {
		t.Errorf("Limit: got %d, want 200", info.Limit)
	}
	if info.Remaining != 150 {
		t.Errorf("Remaining: got %d, want 150", info.Remaining)
	}
	if info.WindowSecs != 30 {
		t.Errorf("WindowSecs: got %d, want 30", info.WindowSecs)
	}
}

func TestParseRateLimitHeaders_XRateLimitLimitVariant(t *testing.T) {
	// Alternative hyphenation: X-Rate-Limit-*
	resp := makeResp(map[string]string{
		"X-Rate-Limit-Limit":     "50",
		"X-Rate-Limit-Remaining": "5",
		"X-Rate-Limit-Window":    "10",
	})
	info := parseRateLimitHeaders(resp)

	if info.Limit != 50 {
		t.Errorf("Limit: got %d, want 50", info.Limit)
	}
	if info.Remaining != 5 {
		t.Errorf("Remaining: got %d, want 5", info.Remaining)
	}
	if info.WindowSecs != 10 {
		t.Errorf("WindowSecs: got %d, want 10", info.WindowSecs)
	}
}

func TestParseRateLimitHeaders_ResetUnixTimestamp(t *testing.T) {
	future := time.Now().Add(30 * time.Second)
	resp := makeResp(map[string]string{
		"X-RateLimit-Reset": fmt.Sprintf("%d", future.Unix()),
	})
	info := parseRateLimitHeaders(resp)

	if info.ResetAt.IsZero() {
		t.Fatal("ResetAt should be set for unix timestamp")
	}
	diff := info.ResetAt.Sub(future)
	if diff < -2*time.Second || diff > 2*time.Second {
		t.Errorf("ResetAt off by %v (expected ~%v, got %v)", diff, future, info.ResetAt)
	}
}

func TestParseRateLimitHeaders_ResetDeltaSeconds(t *testing.T) {
	// A small number (< 1_000_000_000) is treated as delta seconds from now.
	before := time.Now()
	resp := makeResp(map[string]string{
		"X-RateLimit-Reset": "45",
	})
	info := parseRateLimitHeaders(resp)
	after := time.Now()

	if info.ResetAt.IsZero() {
		t.Fatal("ResetAt should be set for delta seconds")
	}
	lo := before.Add(44 * time.Second)
	hi := after.Add(46 * time.Second)
	if info.ResetAt.Before(lo) || info.ResetAt.After(hi) {
		t.Errorf("ResetAt %v out of expected range [%v, %v]", info.ResetAt, lo, hi)
	}
}

func TestParseRateLimitHeaders_RateLimitResetVariant(t *testing.T) {
	future := time.Now().Add(60 * time.Second)
	resp := makeResp(map[string]string{
		"RateLimit-Reset": fmt.Sprintf("%d", future.Unix()),
	})
	info := parseRateLimitHeaders(resp)

	if info.ResetAt.IsZero() {
		t.Fatal("ResetAt should be set from RateLimit-Reset")
	}
}

func TestParseRateLimitHeaders_RetryAfter(t *testing.T) {
	resp := makeResp(map[string]string{
		"Retry-After": "15",
	})
	info := parseRateLimitHeaders(resp)

	if info.RetryAfter != 15 {
		t.Errorf("RetryAfter: got %d, want 15", info.RetryAfter)
	}
}

func TestParseRateLimitHeaders_RemainingZero(t *testing.T) {
	// Remaining=0 is valid and must not be confused with "absent" (-1).
	resp := makeResp(map[string]string{
		"X-RateLimit-Limit":     "10",
		"X-RateLimit-Remaining": "0",
	})
	info := parseRateLimitHeaders(resp)

	if info.Remaining != 0 {
		t.Errorf("Remaining: got %d, want 0", info.Remaining)
	}
}

func TestParseRateLimitHeaders_Empty(t *testing.T) {
	resp := makeResp(nil)
	info := parseRateLimitHeaders(resp)

	if info.Limit != -1 {
		t.Errorf("Limit: got %d, want -1", info.Limit)
	}
	if info.Remaining != -1 {
		t.Errorf("Remaining: got %d, want -1", info.Remaining)
	}
	if info.WindowSecs != -1 {
		t.Errorf("WindowSecs: got %d, want -1", info.WindowSecs)
	}
	if info.RetryAfter != -1 {
		t.Errorf("RetryAfter: got %d, want -1", info.RetryAfter)
	}
	if !info.ResetAt.IsZero() {
		t.Errorf("ResetAt should be zero for empty headers")
	}
}

// ─── classifyWindowType ───────────────────────────────────────────────────────

func TestClassifyWindowType_NoSamples(t *testing.T) {
	wt, reason := classifyWindowType(nil)
	if wt != "Unknown" {
		t.Errorf("got %q, want Unknown", wt)
	}
	if reason == "" {
		t.Error("reason should not be empty")
	}
}

func TestClassifyWindowType_EmptySamples(t *testing.T) {
	wt, _ := classifyWindowType([]RateLimitInfo{})
	if wt != "Unknown" {
		t.Errorf("got %q, want Unknown", wt)
	}
}

func TestClassifyWindowType_RollingWindowByWindowHeader(t *testing.T) {
	// X-RateLimit-Window present → immediate Rolling Window identification.
	samples := []RateLimitInfo{
		{Limit: 100, Remaining: 50, WindowSecs: 60},
	}
	wt, method := classifyWindowType(samples)
	if wt != "Rolling Window" {
		t.Errorf("got %q, want Rolling Window", wt)
	}
	if !strings.Contains(method, "window field") {
		t.Errorf("method %q should mention window field", method)
	}
}

func TestClassifyWindowType_FixedWindowByConstantReset(t *testing.T) {
	// Constant reset timestamp across samples → Fixed Window.
	reset := time.Now().Add(30 * time.Second)
	samples := []RateLimitInfo{
		{Limit: 100, ResetAt: reset},
		{Limit: 100, ResetAt: reset.Add(500 * time.Millisecond)}, // within 2s tolerance
		{Limit: 100, ResetAt: reset.Add(-500 * time.Millisecond)},
	}
	wt, method := classifyWindowType(samples)
	if wt != "Fixed Window" {
		t.Errorf("got %q, want Fixed Window", wt)
	}
	if !strings.Contains(method, "constant reset") {
		t.Errorf("method %q should mention constant reset", method)
	}
}

func TestClassifyWindowType_RollingWindowByAdvancingReset(t *testing.T) {
	// Monotonically increasing reset timestamps → Rolling Window.
	base := time.Now().Add(30 * time.Second)
	samples := []RateLimitInfo{
		{ResetAt: base},
		{ResetAt: base.Add(3 * time.Second)},
		{ResetAt: base.Add(6 * time.Second)},
	}
	wt, method := classifyWindowType(samples)
	if wt != "Rolling Window" {
		t.Errorf("got %q, want Rolling Window", wt)
	}
	if !strings.Contains(method, "advances") {
		t.Errorf("method %q should mention advancing reset", method)
	}
}

func TestClassifyWindowType_InsufficientResets(t *testing.T) {
	// One sample without ResetAt → Unknown (need ≥2 reset samples to classify).
	samples := []RateLimitInfo{
		{Limit: 100, Remaining: 50},
	}
	wt, _ := classifyWindowType(samples)
	if wt != "Unknown" {
		t.Errorf("got %q, want Unknown", wt)
	}
}

func TestClassifyWindowType_OnlyOneResetSample(t *testing.T) {
	samples := []RateLimitInfo{
		{ResetAt: time.Now().Add(30 * time.Second)},
	}
	wt, _ := classifyWindowType(samples)
	if wt != "Unknown" {
		t.Errorf("got %q, want Unknown", wt)
	}
}

func TestClassifyWindowType_AmbiguousReset(t *testing.T) {
	// Non-monotonic resets that also differ > 2s → Ambiguous → Unknown.
	base := time.Now().Add(30 * time.Second)
	samples := []RateLimitInfo{
		{ResetAt: base},
		{ResetAt: base.Add(10 * time.Second)}, // advancing
		{ResetAt: base.Add(5 * time.Second)},  // goes backward
	}
	wt, _ := classifyWindowType(samples)
	if wt != "Unknown" {
		t.Errorf("got %q, want Unknown", wt)
	}
}

func TestClassifyWindowType_WindowHeaderTakesPriorityOverReset(t *testing.T) {
	// If WindowSecs is set, we should classify as Rolling Window without
	// needing to inspect reset timestamps.
	reset := time.Now().Add(30 * time.Second)
	samples := []RateLimitInfo{
		// constant reset would normally → Fixed Window, but WindowSecs wins.
		{WindowSecs: 30, ResetAt: reset},
		{WindowSecs: 30, ResetAt: reset.Add(200 * time.Millisecond)},
	}
	wt, _ := classifyWindowType(samples)
	if wt != "Rolling Window" {
		t.Errorf("got %q, want Rolling Window", wt)
	}
}
