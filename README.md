# proxypool

[![Go Reference](https://pkg.go.dev/badge/github.com/Mootjelh/proxypool.svg)](https://pkg.go.dev/github.com/Mootjelh/proxypool)
[![Go Report Card](https://goreportcard.com/badge/github.com/Mootjelh/proxypool)](https://goreportcard.com/report/github.com/Mootjelh/proxypool)

A rotating proxy pool with cooldowns, for Go.

No dependencies. No background goroutines. Safe for concurrent use.

## Why

Every project that runs more than one proxy rewrites the same four things, each individually trivial and collectively a morning:

- providers emit at least four different line formats, and **none of them is the URL an HTTP client accepts**
- a blocked address should stop being handed out for a while, then come back on its own
- credentials must never reach a log
- when every address is cooling down, the caller needs to be *told* rather than handed one anyway

That is the whole scope. There are no health checks and no scoring, because whether an address is usable is something only your own traffic can answer — a liveness probe that passes proves nothing about the request you actually care about.

## Install

```bash
go get github.com/Mootjelh/proxypool
```

## Use

```go
pool, err := proxypool.Load("proxies.txt")
if err != nil {
    return err
}

for {
    p, healthy := pool.Next()
    if !healthy {
        // Every address is cooling down. p is the one recovering soonest.
        time.Sleep(5 * time.Second)
        continue
    }

    transport, err := p.Transport()
    if err != nil {
        return err
    }
    client := &http.Client{Transport: transport, Timeout: 20 * time.Second}

    resp, err := client.Get(target)
    if err != nil || resp.StatusCode == http.StatusTooManyRequests {
        log.Printf("blocked on %s", p)   // credentials stripped, index kept
        pool.Block(p, 10*time.Minute)
        continue
    }
    ...
}
```

## Input formats

`Normalize` takes what providers hand out:

| input | result |
|---|---|
| `1.2.3.4:8080` | `http://1.2.3.4:8080` |
| `gw.example.com:7000:user:pass` | `http://user:pass@gw.example.com:7000` |
| `user:pass@1.2.3.4:8080` | `http://user:pass@1.2.3.4:8080` |
| `socks5://user:pass@host:1080` | unchanged |

`Load` reads a file of them, skipping blanks and `#` comments. **A line it cannot parse is an error naming the line number**, not a silent skip — a proxy list is configuration, and quietly running on nine of ten addresses is worse than not starting at all.

## Two things this gets right that are easy to get wrong

### Logging a pool whose entries share a host

Credentials must be stripped from logs. But sticky residential pools give every entry the **same `host:port`** and vary only the username, which is what encodes the session. Strip the credentials and a pool of a thousand prints as one line, repeated — so a log showing a healthy rotation is indistinguishable from a log showing every request pinned to one address.

`Proxy.String()` therefore keeps the index:

```
pool#0 gw.example.com:5555
pool#1 gw.example.com:5555
pool#2 gw.example.com:5555
```

### The BOM in your proxy list

PowerShell's `Out-File` and `Set-Content` write UTF-8 **with a BOM** by default. Those three invisible bytes attach to the first address only, so `gw.example.com` becomes a hostname that does not resolve — and it presents as that one proxy being dead, which is exactly the kind of thing you would never think to check.

`Load` strips it. There is a test for it.

## Ordering matters in the parser, and here is why

A password may legitimately contain an `@`. So `host:port:user:p@ss` gets checked by **field count before** anything searches for an `@` — otherwise it is read as `user:pass@host:port` and yields a URL pointing at a host that was never in your file.

Field count is unambiguous where a character search is not. This one is pinned by a test, because it is the sort of thing a later refactor "simplifies" straight back into a bug.

## What it does not do

- **No health checking.** Use your own traffic.
- **No per-request retry.** That is your control flow, not the pool's.
- **No SOCKS dialer.** `Proxy.Transport()` hands `socks5://` URLs to `net/http`, which does support them; anything more exotic is yours to build from `Proxy.URL`.
- **No rotation strategies beyond round robin.** If you want one address pinned per long-lived session, use `Pool.All()` and index it yourself — that is a different strategy and stacking it on `Next()` gets you neither.

## License

MIT
