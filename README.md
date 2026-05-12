
# Install and build

```bash
git clone https://github.com/royz/rate-limit-checker
cd rate-limit-checker
go build rlc.go
```

# Usage

```
Usage:
  rlc -u https://example.com -m GET -c 5000 -t 20

  -m string
        method (default "HEAD")
  -c int
        count (default 1000)
  -s  boolean
        continue after the code changing
  -t int
        threads (default 10)
  -u string
        url
```
