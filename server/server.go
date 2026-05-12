package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ─── Rolling Window ────────────────────────────────────────────────────────────
// Tracks per-key request timestamps and prunes those older than the window.

type rollingWindowBucket struct {
	mu         sync.Mutex
	timestamps []time.Time
	window     time.Duration
	limit      int
}

var (
	rwMu      sync.RWMutex
	rwBuckets = make(map[string]*rollingWindowBucket)
)

func getRollingBucket(key string, window time.Duration, limit int) *rollingWindowBucket {
	rwMu.RLock()
	b, ok := rwBuckets[key]
	rwMu.RUnlock()
	if ok {
		return b
	}
	rwMu.Lock()
	defer rwMu.Unlock()
	if b, ok = rwBuckets[key]; ok {
		return b
	}
	b = &rollingWindowBucket{window: window, limit: limit}
	rwBuckets[key] = b
	return b
}

func rollingWindowHandler(w http.ResponseWriter, r *http.Request) {
	windowSecs, limit := parseWindowLimit(r, 60, 100)
	key := fmt.Sprintf("%d:%d", windowSecs, limit)
	window := time.Duration(windowSecs) * time.Second
	b := getRollingBucket(key, window, limit)

	b.mu.Lock()
	now := time.Now()
	cutoff := now.Add(-window)

	j := 0
	for _, t := range b.timestamps {
		if t.After(cutoff) {
			b.timestamps[j] = t
			j++
		}
	}
	b.timestamps = b.timestamps[:j]

	count := len(b.timestamps)
	resetAt := now.Add(window)
	if count > 0 {
		resetAt = b.timestamps[0].Add(window)
	}

	if count >= limit {
		retryAfter := int(math.Ceil(time.Until(resetAt).Seconds()))
		b.mu.Unlock()
		if wantHeaders(r) {
			setRateLimitHeaders(w, limit, 0, resetAt, windowSecs, retryAfter)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		writeJSON(w, "rate limit exceeded")
		return
	}

	b.timestamps = append(b.timestamps, now)
	remaining := limit - count - 1
	b.mu.Unlock()

	if wantHeaders(r) {
		setRateLimitHeaders(w, limit, remaining, resetAt, windowSecs, -1)
	}
	w.WriteHeader(http.StatusOK)
	writeJSON(w, "ok")
}

// ─── Fixed Window ──────────────────────────────────────────────────────────────
// Divides time into hard buckets; counter resets at each boundary.

type fixedWindowBucket struct {
	mu          sync.Mutex
	windowStart time.Time
	count       int
	window      time.Duration
	limit       int
}

var (
	fwMu      sync.RWMutex
	fwBuckets = make(map[string]*fixedWindowBucket)
)

func getFixedBucket(key string, window time.Duration, limit int) *fixedWindowBucket {
	fwMu.RLock()
	b, ok := fwBuckets[key]
	fwMu.RUnlock()
	if ok {
		return b
	}
	fwMu.Lock()
	defer fwMu.Unlock()
	if b, ok = fwBuckets[key]; ok {
		return b
	}
	b = &fixedWindowBucket{window: window, limit: limit, windowStart: time.Now()}
	fwBuckets[key] = b
	return b
}

func fixedWindowHandler(w http.ResponseWriter, r *http.Request) {
	windowSecs, limit := parseWindowLimit(r, 60, 100)
	key := fmt.Sprintf("%d:%d", windowSecs, limit)
	window := time.Duration(windowSecs) * time.Second
	b := getFixedBucket(key, window, limit)

	b.mu.Lock()
	now := time.Now()
	if now.After(b.windowStart.Add(b.window)) {
		b.windowStart = now
		b.count = 0
	}
	resetAt := b.windowStart.Add(b.window)

	if b.count >= b.limit {
		retryAfter := int(math.Ceil(time.Until(resetAt).Seconds()))
		b.mu.Unlock()
		if wantHeaders(r) {
			setRateLimitHeaders(w, limit, 0, resetAt, windowSecs, retryAfter)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		writeJSON(w, "rate limit exceeded")
		return
	}

	b.count++
	remaining := b.limit - b.count
	b.mu.Unlock()

	if wantHeaders(r) {
		setRateLimitHeaders(w, limit, remaining, resetAt, windowSecs, -1)
	}
	w.WriteHeader(http.StatusOK)
	writeJSON(w, "ok")
}

// ─── Token Bucket ──────────────────────────────────────────────────────────────
// Tokens refill continuously at `rate`/s up to `burst`.
// Query params: rate=10&burst=50

type tokenBucketState struct {
	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
	rate       float64
	burst      float64
}

var (
	tbMu      sync.RWMutex
	tbBuckets = make(map[string]*tokenBucketState)
)

func getTokenBucket(key string, rate, burst float64) *tokenBucketState {
	tbMu.RLock()
	b, ok := tbBuckets[key]
	tbMu.RUnlock()
	if ok {
		return b
	}
	tbMu.Lock()
	defer tbMu.Unlock()
	if b, ok = tbBuckets[key]; ok {
		return b
	}
	b = &tokenBucketState{tokens: burst, lastRefill: time.Now(), rate: rate, burst: burst}
	tbBuckets[key] = b
	return b
}

func tokenBucketHandler(w http.ResponseWriter, r *http.Request) {
	rate := parseFloat(r, "rate", 10)
	burst := parseFloat(r, "burst", 50)
	key := fmt.Sprintf("%.4f:%.4f", rate, burst)
	b := getTokenBucket(key, rate, burst)

	b.mu.Lock()
	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens = math.Min(b.burst, b.tokens+elapsed*b.rate)
	b.lastRefill = now

	if b.tokens < 1 {
		retryAfter := int(math.Ceil((1 - b.tokens) / b.rate))
		b.mu.Unlock()
		if wantHeaders(r) {
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(int(burst)))
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		}
		w.WriteHeader(http.StatusTooManyRequests)
		writeJSON(w, "rate limit exceeded")
		return
	}

	b.tokens--
	remaining := int(b.tokens)
	b.mu.Unlock()

	if wantHeaders(r) {
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(int(burst)))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	}
	w.WriteHeader(http.StatusOK)
	writeJSON(w, "ok")
}

// ─── Leaky Bucket ─────────────────────────────────────────────────────────────
// Requests queue up; queue drains at `rate`/s. Returns 429 when full.
// Query params: rate=10&capacity=100

type leakyBucketState struct {
	mu        sync.Mutex
	queue     float64
	lastDrain time.Time
	rate      float64
	capacity  int
}

var (
	lbMu      sync.RWMutex
	lbBuckets = make(map[string]*leakyBucketState)
)

func getLeakyBucket(key string, rate float64, capacity int) *leakyBucketState {
	lbMu.RLock()
	b, ok := lbBuckets[key]
	lbMu.RUnlock()
	if ok {
		return b
	}
	lbMu.Lock()
	defer lbMu.Unlock()
	if b, ok = lbBuckets[key]; ok {
		return b
	}
	b = &leakyBucketState{queue: 0, lastDrain: time.Now(), rate: rate, capacity: capacity}
	lbBuckets[key] = b
	return b
}

func leakyBucketHandler(w http.ResponseWriter, r *http.Request) {
	rate := parseFloat(r, "rate", 10)
	capacity, _ := strconv.Atoi(r.URL.Query().Get("capacity"))
	if capacity <= 0 {
		capacity = 100
	}
	key := fmt.Sprintf("%.4f:%d", rate, capacity)
	b := getLeakyBucket(key, rate, capacity)

	b.mu.Lock()
	now := time.Now()
	elapsed := now.Sub(b.lastDrain).Seconds()
	b.queue = math.Max(0, b.queue-elapsed*b.rate)
	b.lastDrain = now

	if b.queue >= float64(b.capacity) {
		retryAfter := int(math.Ceil((b.queue - float64(b.capacity) + 1) / b.rate))
		b.mu.Unlock()
		if wantHeaders(r) {
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(capacity))
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		}
		w.WriteHeader(http.StatusTooManyRequests)
		writeJSON(w, "rate limit exceeded")
		return
	}

	b.queue++
	remaining := int(float64(b.capacity) - b.queue)
	b.mu.Unlock()

	if wantHeaders(r) {
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(capacity))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	}
	w.WriteHeader(http.StatusOK)
	writeJSON(w, "ok")
}

// ─── Reset ────────────────────────────────────────────────────────────────────
// DELETE /reset  - clears all in-memory rate limit state.

func resetHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "use DELETE /reset", http.StatusMethodNotAllowed)
		return
	}
	rwMu.Lock()
	rwBuckets = make(map[string]*rollingWindowBucket)
	rwMu.Unlock()

	fwMu.Lock()
	fwBuckets = make(map[string]*fixedWindowBucket)
	fwMu.Unlock()

	tbMu.Lock()
	tbBuckets = make(map[string]*tokenBucketState)
	tbMu.Unlock()

	lbMu.Lock()
	lbBuckets = make(map[string]*leakyBucketState)
	lbMu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func wantHeaders(r *http.Request) bool {
	return r.URL.Query().Get("headers") == "true"
}

func setRateLimitHeaders(w http.ResponseWriter, limit, remaining int, resetAt time.Time, windowSecs, retryAfter int) {
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))
	w.Header().Set("X-RateLimit-Window", strconv.Itoa(windowSecs))
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
}

func parseWindowLimit(r *http.Request, defaultWindow, defaultLimit int) (int, int) {
	q := r.URL.Query()
	windowSecs, _ := strconv.Atoi(q.Get("window"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if windowSecs <= 0 {
		windowSecs = defaultWindow
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	return windowSecs, limit
}

func parseFloat(r *http.Request, key string, defaultVal float64) float64 {
	v, err := strconv.ParseFloat(r.URL.Query().Get(key), 64)
	if err != nil || v <= 0 {
		return defaultVal
	}
	return v
}

func writeJSON(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/rolling-window", rollingWindowHandler)
	mux.HandleFunc("/fixed-window", fixedWindowHandler)
	mux.HandleFunc("/token-bucket", tokenBucketHandler)
	mux.HandleFunc("/leaky-bucket", leakyBucketHandler)
	mux.HandleFunc("/reset", resetHandler)

	addr := ":8080"
	fmt.Printf("Rate limit test server on %s\n\n", addr)
	fmt.Println("Endpoints:")
	fmt.Println("  GET  /rolling-window?window=30&limit=500   sliding window (window in seconds)")
	fmt.Println("  GET  /fixed-window?window=30&limit=500     hard-reset window (window in seconds)")
	fmt.Println("  GET  /token-bucket?rate=10&burst=50        token bucket (rate = tokens/sec)")
	fmt.Println("  GET  /leaky-bucket?rate=10&capacity=100    leaky bucket (rate = drain/sec)")
	fmt.Println("  DEL  /reset                                clear all state")
	fmt.Println()

	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Println("server error:", err)
	}
}
