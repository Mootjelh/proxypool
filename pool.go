// Package proxypool hands out proxies round-robin and sidelines the ones that
// got blocked.
//
// It solves the small set of problems that show up every time you run more than
// one proxy, and that are individually trivial and collectively tedious:
//
//   - providers emit at least four different line formats, and none of them is
//     the URL an HTTP client wants
//   - a blocked address should stop being handed out for a while, then come
//     back on its own
//   - credentials must never reach a log
//   - when every address is cooling down, the caller needs to know that rather
//     than be handed one silently
//
// It is deliberately small: no health checks, no scoring, no background
// goroutines. A [Pool] is a picker with a cooldown, and whether an address is
// healthy is something only the caller's own traffic can answer.
//
// All methods are safe for concurrent use.
package proxypool

import (
	"bufio"
	"fmt"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Pool hands out proxies round-robin and sidelines ones that got blocked.
// The zero Pool is not usable; build one with [New] or [Load].
type Pool struct {
	mu      sync.Mutex
	entries []*entry
	idx     int
}

type entry struct {
	url          string
	blockedUntil time.Time
}

// Proxy is one address from the pool.
type Proxy struct {
	// URL is the full proxy URL, credentials included. Pass it to your HTTP
	// client — and do not log it. Use the Proxy itself for that.
	URL string

	// Index is the address's stable position in the pool, counting from 0.
	Index int
}

// IsDirect reports whether this entry means "no proxy, connect directly".
func (p Proxy) IsDirect() bool { return p.URL == "" }

// String renders the proxy for logs: credentials stripped, index kept.
//
// The index is not decoration. Rotating residential pools typically give every
// entry the SAME host:port and vary only the username, which encodes the sticky
// session. Strip the credentials — which you must — and a pool of a thousand
// addresses prints as one, so a log that says the rotation is working looks
// exactly like a log that says it is not.
func (p Proxy) String() string {
	if p.IsDirect() {
		return "direct"
	}
	return fmt.Sprintf("pool#%d %s", p.Index, Mask(p.URL))
}

// Transport returns an *http.Transport routed through this proxy, or a plain
// one when the entry is direct.
func (p Proxy) Transport() (*http.Transport, error) {
	t := &http.Transport{}
	if p.IsDirect() {
		return t, nil
	}
	u, err := neturl.Parse(p.URL)
	if err != nil {
		return nil, fmt.Errorf("proxypool: %q: %w", Mask(p.URL), err)
	}
	t.Proxy = http.ProxyURL(u)
	return t, nil
}

// New builds a pool from already-normalised proxy URLs.
//
// An empty slice, or a slice containing one empty string, means "connect
// directly" — so a caller that has no proxies configured can use the same code
// path as one that does, rather than branching everywhere.
func New(urls []string) *Pool {
	if len(urls) == 0 {
		urls = []string{""}
	}
	entries := make([]*entry, 0, len(urls))
	for _, u := range urls {
		entries = append(entries, &entry{url: u})
	}
	return &Pool{entries: entries}
}

// Load reads a proxy list from disk, one entry per line, and normalises each
// one. Blank lines and lines starting with # are ignored.
//
// An unparseable line is an error naming the line number, rather than a silent
// skip: a proxy list is configuration, and quietly running on nine of ten
// addresses is worse than not starting.
func Load(path string) (*Pool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var urls []string
	scanner := bufio.NewScanner(f)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		// Strip the UTF-8 BOM as well as whitespace.
		//
		// Not pedantry. PowerShell's Out-File and Set-Content write UTF-8 WITH a
		// BOM by default, so a proxy list produced on Windows has three
		// invisible bytes glued to the FIRST address. That turns
		// "gw.example.com" into "\ufeffgw.example.com", which is a different
		// hostname — and the failure is silent, affects exactly one entry, and
		// presents as that one proxy being dead.
		//
		// The BOM is written as an escape rather than a literal here because a
		// literal one is itself a Go compile error: "illegal byte order mark".
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		u, err := Normalize(line)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, lineNo, err)
		}
		urls = append(urls, u)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("%s: no proxies found", path)
	}
	return New(urls), nil
}

// Normalize converts the common proxy list formats into a URL an HTTP client
// accepts:
//
//	host:port                    ->  http://host:port
//	host:port:user:pass          ->  http://user:pass@host:port
//	user:pass@host:port          ->  http://user:pass@host:port
//	scheme://...                 ->  unchanged
//
// The third form is what most residential providers emit by default, and it is
// the one no HTTP client takes.
func Normalize(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("empty proxy entry")
	}

	// Already a URL: trust it as-is, including socks5:// and https://.
	if i := strings.Index(s, "://"); i > 0 {
		return s, nil
	}

	// Colon count is checked BEFORE looking for an "@", and the order matters:
	// a password may legitimately contain an "@", so "host:port:user:p@ss"
	// would otherwise be mistaken for the user:pass@host:port form and produce
	// a URL pointing at a hostname that does not exist. Field count is
	// unambiguous where a character search is not.
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 4: // host:port:user:pass
		host, port, user, pass := parts[0], parts[1], parts[2], parts[3]
		// Built through url.URL rather than concatenated, so a credential
		// containing a reserved character is escaped rather than producing a
		// URL that parses into something else.
		u := &neturl.URL{
			Scheme: "http",
			User:   neturl.UserPassword(user, pass),
			Host:   host + ":" + port,
		}
		return u.String(), nil
	case 2: // host:port
		return "http://" + parts[0] + ":" + parts[1], nil
	}

	if strings.Contains(s, "@") { // user:pass@host:port
		return "http://" + s, nil
	}

	return "", fmt.Errorf("unrecognised proxy format %q (want host:port, host:port:user:pass, user:pass@host:port, or a URL)", raw)
}

// Next returns the proxy to use for the next request.
//
// The bool reports whether it is currently healthy. When every proxy is cooling
// down it returns the one that recovers soonest, with false — the caller is
// better placed than the pool to decide between waiting, proceeding anyway and
// giving up.
func (p *Pool) Next() (Proxy, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	n := len(p.entries)

	for i := 0; i < n; i++ {
		idx := p.idx % n
		e := p.entries[idx]
		p.idx = (p.idx + 1) % n
		if e.blockedUntil.Before(now) {
			return Proxy{URL: e.url, Index: idx}, true
		}
	}

	soonest, soonestIdx := p.entries[0], 0
	for i, e := range p.entries[1:] {
		if e.blockedUntil.Before(soonest.blockedUntil) {
			soonest, soonestIdx = e, i+1
		}
	}
	return Proxy{URL: soonest.url, Index: soonestIdx}, false
}

// Block sidelines a proxy for the given duration after it was throttled.
//
// A shorter Block never shortens a cooldown already in place. Two failures on
// one address should not leave it less penalised than one.
func (p *Pool) Block(pr Proxy, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	until := time.Now().Add(d)
	if pr.Index >= 0 && pr.Index < len(p.entries) && p.entries[pr.Index].url == pr.URL {
		if p.entries[pr.Index].blockedUntil.Before(until) {
			p.entries[pr.Index].blockedUntil = until
		}
		return
	}
	// Fall back to matching by URL, so a Proxy built by hand still works.
	for _, e := range p.entries {
		if e.url == pr.URL && e.blockedUntil.Before(until) {
			e.blockedUntil = until
			return
		}
	}
}

// Len is the total number of proxies in the pool.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// Healthy is the number of proxies not currently cooling down.
func (p *Pool) Healthy() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	count := 0
	for _, e := range p.entries {
		if e.blockedUntil.Before(now) {
			count++
		}
	}
	return count
}

// All returns every proxy in the pool, in file order, regardless of cooldown.
//
// Use it when you want the list rather than the picker — for instance to pin
// one address per long-lived session, which is a different rotation strategy
// from this pool's per-request round robin and should not be built on top of
// [Pool.Next].
func (p *Pool) All() []Proxy {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]Proxy, 0, len(p.entries))
	for i, e := range p.entries {
		out = append(out, Proxy{URL: e.url, Index: i})
	}
	return out
}

// URLs returns the pool's addresses, in file order.
func (p *Pool) URLs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]string, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, e.url)
	}
	return out
}

// Mask strips credentials and scheme from a proxy URL so logins never reach a
// log. It is what [Proxy.String] uses.
//
// Prefer logging the Proxy itself: on a pool whose entries share a host, a
// masked URL alone cannot tell them apart.
func Mask(raw string) string {
	if raw == "" {
		return "direct"
	}
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if at := strings.LastIndex(s, "@"); at >= 0 {
		s = s[at+1:]
	}
	return s
}
