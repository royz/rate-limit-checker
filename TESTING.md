# Running Tests

## Unit tests

Tests for `parseRateLimitHeaders` and `classifyWindowType` - no server required, run instantly.

```bash
go test -v -run "TestParseRateLimitHeaders|TestClassifyWindowType" .
```

## Integration tests

`TestMain` compiles both binaries, starts the test server on a random free port, and tears everything down when done.

**All integration tests (~25s total):**

```bash
go test -v -timeout 180s .
```

**Header-based only (fast, ~5s):**

```bash
go test -v -run "TestRollingWindow_HeaderBased|TestFixedWindow_HeaderBased|TestTokenBucket_HeaderBased|TestLeakyBucket_HeaderBased" -timeout 60s .
```

**Behavioral probe only (~20s - probes wait for rate-limit windows to expire):**

```bash
go test -v -run "TestRollingWindow_BehavioralProbeInvoked|TestFixedWindow_BehavioralProbeInvoked|TestFixedWindow_BehavioralType" -timeout 180s .
```

## Test descriptions

| Test | Type | What it covers |
|---|---|---|
| `TestParseRateLimitHeaders_*` | Unit | Header parsing for all three naming conventions (`X-RateLimit-*`, `RateLimit-*`, `X-Rate-Limit-*`), Unix-timestamp reset, delta-seconds reset, `Retry-After`, `Remaining=0`, empty headers |
| `TestClassifyWindowType_*` | Unit | Window classification from samples: explicit window header, constant reset (Fixed), advancing reset (Rolling), edge cases (nil, one sample, ambiguous) |
| `TestRollingWindow_HeaderBased` | Integration | Rolling window detected from `X-RateLimit-Window` header |
| `TestFixedWindow_HeaderBased` | Integration | Fixed window classified from headers, header-based detection method |
| `TestTokenBucket_HeaderBased` | Integration | Token bucket triggers 429; `Limit` value read from `X-RateLimit-Limit` |
| `TestLeakyBucket_HeaderBased` | Integration | Leaky bucket triggers 429; `Limit` value read from `X-RateLimit-Limit` |
| `TestRollingWindow_BehavioralProbeInvoked` | Integration | Behavioral probe runs when no headers are present (rolling window) |
| `TestFixedWindow_BehavioralProbeInvoked` | Integration | Behavioral probe runs when no headers are present (fixed window) |
| `TestFixedWindow_BehavioralType` | Integration | Serialised burst (1 thread) lets probe observe sharp full reset → Fixed Window |
