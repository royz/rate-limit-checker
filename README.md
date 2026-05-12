
# Install and build

```bash
git clone https://github.com/royz/rate-limit-checker
cd rate-limit-checker
go build -o rlc rlc.go
```

# Usage

```bash
./rlc -u https://example.com -m GET -c 5000 -t 20
```

| Short | Long | Type | Default | Description |
|---|---|---|---|---|
| `-u` | `--url` | string | | Target URL |
| `-m` | `--method` | string | `HEAD` | HTTP method |
| `-c` | `--count` | int | `1000` | Number of requests to send |
| `-t` | `--threads` | int | `10` | Number of concurrent threads |
| `-s` | `--skip` | bool | `false` | Continue after the response code changes |
| `-d` | `--detect` | bool | `true` | Detect rate limit type and window info |
| `-i` | `--probe-interval` | int | `1000` | ms between behavioral probe requests (used when headers are absent) |
| `-p` | `--probe-duration` | int | `30` | Seconds to probe for recovery after rate limit is hit |

# Test server

A local test server is included in `server/server.go`. It implements four
rate-limit algorithms in memory so you can verify detection without hitting a
real API.

## Start the server

```bash
go run ./server/server.go
# listening on :8080
```

## Endpoints

| Endpoint | Query params | Algorithm |
|---|---|---|
| `GET /rolling-window` | `window=<secs>&limit=<n>` | Sliding window - counts requests in the last N seconds |
| `GET /fixed-window` | `window=<secs>&limit=<n>` | Hard-reset window - counter resets at each boundary |
| `GET /token-bucket` | `rate=<n/s>&burst=<n>` | Token bucket - refills at `rate` tokens/sec up to `burst` |
| `GET /leaky-bucket` | `rate=<n/s>&capacity=<n>` | Leaky bucket - queue drains at `rate`/sec, 429 when full |
| `DELETE /reset` | - | Clear all in-memory state |

All endpoints accept an optional `headers=true` query parameter. When present,
the response includes standard rate limit headers (`X-RateLimit-Limit`,
`X-RateLimit-Remaining`, `X-RateLimit-Reset`, `X-RateLimit-Window`,
`Retry-After`) that `rlc --detect` consumes automatically. Without it, only
the HTTP status code (200 / 429) is returned — useful for testing behavioral
detection without header hints.

## Example: test rolling window (30 s window, 50 req limit)

```bash
# terminal 1 - server
go run ./server/server.go

# terminal 2 - no headers (behavioral detection)
rlc -u "http://localhost:8080/rolling-window?window=30&limit=50" -m GET -c 200 -t 10

# terminal 2 - with headers (header-based detection)
rlc -u "http://localhost:8080/rolling-window?window=30&limit=50&headers=true" -m GET -c 200 -t 10
```

## Example: test fixed window (60 s window, 100 req limit)

```bash
rlc -u "http://localhost:8080/fixed-window?window=60&limit=100&headers=true" -m GET -c 300 -t 15
```

## Example: test token bucket (10 tokens/s, burst of 30)

```bash
rlc -u "http://localhost:8080/token-bucket?rate=10&burst=30&headers=true" -m GET -c 200 -t 5
```

## Example: test leaky bucket (5 drain/s, capacity 20)

```bash
rlc -u "http://localhost:8080/leaky-bucket?rate=5&capacity=20&headers=true" -m GET -c 100 -t 10
```

## Reset state between runs

```bash
curl -X DELETE http://localhost:8080/reset
```
