package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var threads int
var method string
var url string
var code int
var cont bool
var detect bool
var probeInterval int
var probeDuration int

var printMu sync.Mutex

// RateLimitInfo holds parsed rate limit headers from a single response.
type RateLimitInfo struct {
	Limit      int
	Remaining  int
	ResetAt    time.Time
	WindowSecs int
	RetryAfter int
}

// detectionState accumulates data across concurrent requests.
type detectionState struct {
	mu               sync.Mutex
	samples          []RateLimitInfo
	rateLimitHitAt   int
	rateLimitHitTime time.Time
	firstReqTime     time.Time
	requestCount     int
}

var state detectionState

func parseRateLimitHeaders(resp *http.Response) RateLimitInfo {
	info := RateLimitInfo{Limit: -1, Remaining: -1, WindowSecs: -1, RetryAfter: -1}

	parseInt := func(keys ...string) int {
		for _, k := range keys {
			if v := resp.Header.Get(k); v != "" {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					return n
				}
			}
		}
		return -1
	}

	info.Limit = parseInt("X-RateLimit-Limit", "RateLimit-Limit", "X-Rate-Limit-Limit")
	info.Remaining = parseInt("X-RateLimit-Remaining", "RateLimit-Remaining", "X-Rate-Limit-Remaining")
	info.WindowSecs = parseInt("X-RateLimit-Window", "RateLimit-Window", "X-Rate-Limit-Window")
	info.RetryAfter = parseInt("Retry-After")

	for _, k := range []string{"X-RateLimit-Reset", "RateLimit-Reset", "X-Rate-Limit-Reset"} {
		if v := resp.Header.Get(k); v != "" {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				if n > 1_000_000_000 { // Unix timestamp
					info.ResetAt = time.Unix(n, 0)
				} else { // delta seconds
					info.ResetAt = time.Now().Add(time.Duration(n) * time.Second)
				}
			}
			break
		}
	}
	return info
}

func classifyWindowType(samples []RateLimitInfo) (string, string) {
	if len(samples) == 0 {
		return "Unknown", "No header samples collected"
	}

	for _, s := range samples {
		if s.WindowSecs > 0 {
			return "Rolling Window", "Header-based (explicit window field present)"
		}
	}

	var resets []time.Time
	for _, s := range samples {
		if !s.ResetAt.IsZero() {
			resets = append(resets, s.ResetAt)
		}
	}
	if len(resets) < 2 {
		return "Unknown", "Insufficient reset-time samples"
	}

	base := resets[0]
	allSame := true
	allAdvancing := true
	for i := 1; i < len(resets); i++ {
		diff := resets[i].Sub(base)
		if diff < 0 {
			diff = -diff
		}
		if diff > 2*time.Second {
			allSame = false
		}
		if !resets[i].After(resets[i-1]) {
			allAdvancing = false
		}
	}

	if allSame {
		return "Fixed Window", "Header-based (constant reset timestamp across requests)"
	}
	if allAdvancing {
		return "Rolling Window", "Header-based (reset timestamp advances with each request)"
	}
	return "Unknown", "Ambiguous reset timestamps"
}

// printStatus overwrites the current terminal line in place.
func printStatus(format string, args ...interface{}) {
	printMu.Lock()
	defer printMu.Unlock()
	fmt.Printf("\r%-80s", fmt.Sprintf(format, args...))
}

// printLine finalizes the current line and moves to a new one.
func printLine(format string, args ...interface{}) {
	printMu.Lock()
	defer printMu.Unlock()
	fmt.Printf("\r%-80s\n", fmt.Sprintf(format, args...))
}

func main() {
	var wg sync.WaitGroup
	channel := make(chan int)
	var times int

	flag.StringVar(&method, "m", "HEAD", "method")
	flag.StringVar(&method, "method", "HEAD", "method")
	flag.IntVar(&threads, "t", 10, "threads")
	flag.IntVar(&threads, "threads", 10, "threads")
	flag.IntVar(&times, "c", 1000, "count")
	flag.IntVar(&times, "count", 1000, "count")
	flag.BoolVar(&cont, "s", false, "continue after the code changing")
	flag.BoolVar(&cont, "skip", false, "continue after the code changing")
	flag.StringVar(&url, "u", "", "url")
	flag.StringVar(&url, "url", "", "url")
	flag.BoolVar(&detect, "detect", true, "detect rate limit type and window info")
	flag.BoolVar(&detect, "d", true, "detect rate limit type and window info")
	flag.IntVar(&probeInterval, "probe-interval", 1000, "ms between behavioral probe requests (used when headers are absent)")
	flag.IntVar(&probeInterval, "i", 1000, "ms between behavioral probe requests (used when headers are absent)")
	flag.IntVar(&probeDuration, "probe-duration", 30, "seconds to probe for recovery after rate limit is hit")
	flag.IntVar(&probeDuration, "p", 30, "seconds to probe for recovery after rate limit is hit")
	flag.Parse()

	if url == "" {
		fmt.Println("URL was not provided")
		return
	}

	req, _ := http.NewRequest(method, url, nil)
	req.Header.Add("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_13_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/77.0.3865.120 Safari/537.36")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		fmt.Println("Initial request failed:", err)
		return
	}
	code = resp.StatusCode
	resp.Body.Close()
	state.firstReqTime = time.Now()

	printLine("[*] Baseline status: %d | Starting burst: %d requests, %d threads, method %s", code, times, threads, method)

	for t := 0; t < threads; t++ {
		go func() {
			for i := range channel {
				wg.Add(1)
				request(i)
				wg.Done()
			}
		}()
	}
	for i := 0; i < times; i++ {
		channel <- i
	}
	close(channel)
	wg.Wait()
	fmt.Println()

	if !detect {
		return
	}

	state.mu.Lock()
	samples := make([]RateLimitInfo, len(state.samples))
	copy(samples, state.samples)
	hitAt := state.rateLimitHitAt
	hitTime := state.rateLimitHitTime
	firstReqTime := state.firstReqTime
	state.mu.Unlock()

	windowType, detectionMethod := classifyWindowType(samples)

	if windowType == "Unknown" && !hitTime.IsZero() {
		printLine("[*] Headers inconclusive - starting behavioral probe (%ds)...", probeDuration)
		windowType, detectionMethod = behavioralProbe(hitTime, firstReqTime)
		fmt.Println()
	}

	// Collect best known header values across all samples.
	best := RateLimitInfo{Limit: -1, Remaining: -1, WindowSecs: -1, RetryAfter: -1}
	for i := len(samples) - 1; i >= 0; i-- {
		s := samples[i]
		if best.Limit < 0 && s.Limit > 0 {
			best.Limit = s.Limit
		}
		if best.Remaining < 0 && s.Remaining >= 0 {
			best.Remaining = s.Remaining
		}
		if best.WindowSecs < 0 && s.WindowSecs > 0 {
			best.WindowSecs = s.WindowSecs
		}
		if best.RetryAfter < 0 && s.RetryAfter > 0 {
			best.RetryAfter = s.RetryAfter
		}
		if best.ResetAt.IsZero() && !s.ResetAt.IsZero() {
			best.ResetAt = s.ResetAt
		}
	}

	fmt.Println("--- Rate Limit Detection Report ---")
	if hitAt > 0 {
		fmt.Printf("Rate limit triggered at  : request #%d\n", hitAt)
	} else {
		fmt.Println("Rate limit triggered at  : not triggered during burst")
	}
	fmt.Printf("Rate limit type          : %s\n", windowType)
	fmt.Printf("Detection method         : %s\n", detectionMethod)
	if best.Limit > 0 {
		fmt.Printf("Limit                    : %d req/window\n", best.Limit)
	}
	if best.Remaining >= 0 {
		fmt.Printf("Remaining (at end)       : %d\n", best.Remaining)
	}
	if best.WindowSecs > 0 {
		fmt.Printf("Window size              : %ds\n", best.WindowSecs)
	}
	if !best.ResetAt.IsZero() {
		fmt.Printf("Resets at                : %s\n", best.ResetAt.UTC().Format(time.RFC3339))
	}
	if best.RetryAfter > 0 {
		fmt.Printf("Retry-After              : %ds\n", best.RetryAfter)
	}
}

// behavioralProbe fires timed requests after a rate limit hit to infer window type.
// It looks for recovery and then immediately fires a small burst to test if the
// full window reset (Fixed Window) or only a single token refilled (Rolling Window).
func behavioralProbe(hitTime, firstReqTime time.Time) (string, string) {
	client := &http.Client{}
	// Anchor the deadline to whichever is later: hitTime or firstReqTime.
	// This ensures the probe outlasts the rolling window, which is measured
	// from when the burst requests were actually made (firstReqTime), not from
	// when the 429 was first received (hitTime).
	anchor := hitTime
	if firstReqTime.After(anchor) {
		anchor = firstReqTime
	}
	deadline := anchor.Add(time.Duration(probeDuration) * time.Second)
	// Also ensure we probe for at least probeDuration from now.
	if minDeadline := time.Now().Add(time.Duration(probeDuration) * time.Second); minDeadline.After(deadline) {
		deadline = minDeadline
	}
	interval := time.Duration(probeInterval) * time.Millisecond
	prevStatus := 429

	doReq := func() (int, error) {
		req, _ := http.NewRequest(method, url, nil)
		req.Header.Add("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_13_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/77.0.3865.120 Safari/537.36")
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}

	for time.Now().Before(deadline) {
		elapsed := int(time.Since(hitTime).Seconds())
		printStatus("[Probe] %ds elapsed | last status: %d", elapsed, prevStatus)

		status, err := doReq()
		if err != nil {
			time.Sleep(interval)
			continue
		}
		prevStatus = status

		if status == code {
			recoveryAt := int(time.Since(hitTime).Seconds())
			printLine("[!] Recovered at %ds - checking burst behavior...", recoveryAt)

			// Fire 2 more immediate requests to test if the full window reset.
			successes := 1
			for j := 0; j < 2; j++ {
				s, err := doReq()
				if err == nil && s == code {
					successes++
				}
			}
			if successes == 3 {
				return "Fixed Window", fmt.Sprintf("Behavioral probe: full burst succeeded after %ds (sharp reset)", recoveryAt)
			}
			return "Rolling Window", fmt.Sprintf("Behavioral probe: partial recovery after %ds (trickle refill)", recoveryAt)
		}
		time.Sleep(interval)
	}
	return "Unknown", "Behavioral probe: no recovery within probe window"
}

func request(i int) {
	client := &http.Client{}
	req, _ := http.NewRequest(method, url, nil)
	req.Header.Add("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_13_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/77.0.3865.120 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		printLine("[!] Error on #%d: %s", i, err.Error())
		return
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		printLine("[!] Body read error on #%d: %s", i, err.Error())
	}
	resp.Body.Close()

	state.mu.Lock()
	state.requestCount++
	count := state.requestCount
	statusChanged := resp.StatusCode != code && state.rateLimitHitAt == 0
	if statusChanged {
		state.rateLimitHitAt = i
		state.rateLimitHitTime = time.Now()
	}
	state.mu.Unlock()

	elapsed := time.Since(state.firstReqTime).Round(time.Millisecond)

	if detect {
		info := parseRateLimitHeaders(resp)
		if info.Limit > 0 || info.Remaining >= 0 || !info.ResetAt.IsZero() {
			state.mu.Lock()
			state.samples = append(state.samples, info)
			state.mu.Unlock()
		}

		rem := ""
		if info.Remaining >= 0 {
			rem = fmt.Sprintf(" | Rem: %d", info.Remaining)
		}
		printStatus("[Burst] #%d | Status: %d | %d bytes | %s%s", count, resp.StatusCode, len(body), elapsed, rem)

		if statusChanged {
			printLine("[!] Rate limit hit at request #%d: %d → %d", i, code, resp.StatusCode)
			if !detect && !cont {
				os.Exit(5)
			}
		}
	} else {
		printStatus("[Burst] #%d | Status: %d | %d bytes | %s", count, resp.StatusCode, len(body), elapsed)
		if resp.StatusCode != code && !cont {
			os.Exit(5)
		}
	}
}
