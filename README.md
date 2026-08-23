# proxypool

[![CI](https://github.com/Mootjelh/proxypool/actions/workflows/ci.yml/badge.svg)](https://github.com/Mootjelh/proxypool/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/Mootjelh/proxypool.svg)](https://pkg.go.dev/github.com/Mootjelh/proxypool)

A rotating proxy pool with cooldowns, for Go.

No dependencies. No background goroutines. Safe for concurrent use.

## Why

Every project that runs more than one proxy ends up rewriting the same four things, each of them small and collectively a morning:

* providers emit at least four different line formats, and none of them is the URL an HTTP client accepts
* a blocked address should stop being handed out for a while, then come back on its own
* credentials must never reach a log
* when every address is cooling down, the caller needs to be told rather than handed one anyway

That's the whole scope. There are no health checks and no scoring, because whether an address is usable is something only your own traffic can answer. A liveness probe that passes proves nothing about the request you actually care about.

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

`Load` reads a file of them, skipping blank lines and `#` comments. A line it can't parse is an error naming the line number rather than a silent skip. A proxy list is configuration, and quietly running on nine of ten addresses is worse than not starting at all.

## Changing the list while it runs

Providers hand out a fresh list every so often. `Replace` swaps it and keeps the cooldown of every address that survives:

```go
if err := pool.Replace(newList); err != nil {
    return err
}
```

Building a second `Pool` would swap the list too, and would forget that four of those addresses were blocked a minute ago. They'd go straight back into rotation, be refused again, and the pool would relearn the same thing on every refresh.

`Add` appends and skips addresses already present, because a duplicate takes a double share of the traffic under round robin. `Remove` drops one. Both refuse a batch containing an unparseable address rather than applying part of it.

## Seeing what the pool is doing

`Len` and `Healthy` give you two numbers, which is not enough to tune anything. `Stats` returns a snapshot per address:

```go
for _, s := range pool.Stats() {
    log.Println(s)
}
```

```
pool#0 gw.example.com:5555  out=412 blocked=3
pool#1 gw.example.com:5555  out=409 blocked=87 cooling 4m20s
pool#2 gw.example.com:5555  out=0 blocked=0 never used
```

Providers differ enormously in how often their addresses get refused, so a pool mixing two of them looks fine in aggregate while half of it fails. The middle line above is that half.

`never used` is worth grepping for. An address that has never been handed out on a pool that has been running for hours usually means the rotation is not reaching it.

## Two details that are easy to get wrong

### Logging a pool whose entries share a host

Credentials have to be stripped from logs. But sticky residential pools give every entry the same `host:port` and vary only the username, which is what encodes the session. Strip the credentials and a pool of a thousand prints as one line repeated, so a log showing a healthy rotation looks identical to a log showing every request pinned to one address.

`Proxy.String()` keeps the index for that reason:

```
pool#0 gw.example.com:5555
pool#1 gw.example.com:5555
pool#2 gw.example.com:5555
```

### The BOM in your proxy list

PowerShell's `Out-File` and `Set-Content` write UTF-8 with a BOM by default. Those three invisible bytes attach to the first address only, so `gw.example.com` becomes a hostname that doesn't resolve, and it presents as that one proxy being dead. Not the sort of thing you'd think to check.

`Load` strips it, and there's a test for it.

## Why the parser checks field count first

A password can legitimately contain an `@`. So `host:port:user:p@ss` is checked by field count before anything searches for an `@`, otherwise it reads as `user:pass@host:port` and yields a URL pointing at a host that was never in your file.

Field count is unambiguous where a character search isn't. There's a test on this one, because it's the sort of thing a later refactor simplifies straight back into a bug.

## What it doesn't do

* No health checking. Use your own traffic.
* No per-request retry. That's your control flow, not the pool's.
* No SOCKS dialer. `Proxy.Transport()` hands `socks5://` URLs to `net/http`, which supports them. Anything more exotic is yours to build from `Proxy.URL`.
* No rotation strategies beyond round robin. If you want one address pinned per long-lived session, use `Pool.All()` and index it yourself. That's a different strategy, and stacking it on `Next()` gets you neither.

## License

MIT
