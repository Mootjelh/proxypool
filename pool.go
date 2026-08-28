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
	"math/rand"
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

	handedOut int
	blocked   int
	lastUsed  time.Time
}

// Proxy is one address from the pool.
type Proxy struct {
	// URL is the full proxy URL, credentials included. Pass it to your HTTP
	// client, and do not log it. Use the Proxy itself for that.
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
// session. Strip the credentials, which you must, and a pool of a thousand
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
// directly", so a caller that has no proxies configured can use the same code
// path as one that does, rather than branching everywhere.
//
// The rotation starts at a random address rather than the first one. A process
// that restarts, which is every deploy and every crash, would otherwise draw
// the same addresses in the same order each time: measured over 200 restarts of
// a 1000-address pool taking 3 requests each, three addresses carried all 600
// requests and the other 997 carried none. Use [Pool.Rotate] when a fixed
// starting point is wanted.
//
// Only the starting point is random. The order is the file order, and an
// address keeps its index, because [Pool.Stats] is worth nothing if a row
// cannot be matched back to a line in the list.
func New(urls []string) *Pool {
	if len(urls) == 0 {
		urls = []string{""}
	}
	entries := make([]*entry, 0, len(urls))
	for _, u := range urls {
		entries = append(entries, &entry{url: u})
	}
	return &Pool{entries: entries, idx: rand.Intn(len(entries))}
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
		// hostname. The failure is silent, affects exactly one entry, and
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
// down it returns the one that recovers soonest, with false. The caller is
// better placed than the pool to decide between waiting, proceeding anyway and
// giving up.
func (p *Pool) Next() (Proxy, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	n := len(p.entries)

	// An empty pool is reachable through Remove and Replace. It reports a
	// direct entry with false, which the caller must not read as "connect
	// directly": the bool is the whole answer, and here it means there is
	// nothing left to hand out.
	if n == 0 {
		return Proxy{Index: -1}, false
	}

	for i := 0; i < n; i++ {
		idx := p.idx % n
		e := p.entries[idx]
		p.idx = (p.idx + 1) % n
		if e.blockedUntil.Before(now) {
			e.handedOut++
			e.lastUsed = now
			return Proxy{URL: e.url, Index: idx}, true
		}
	}

	soonest, soonestIdx := p.entries[0], 0
	for i, e := range p.entries[1:] {
		if e.blockedUntil.Before(soonest.blockedUntil) {
			soonest, soonestIdx = e, i+1
		}
	}
	// Counted as a hand-out too. The caller receives this address and may well
	// use it, so leaving it out would understate exactly the address that is
	// carrying an exhausted pool.
	soonest.handedOut++
	soonest.lastUsed = now
	return Proxy{URL: soonest.url, Index: soonestIdx}, false
}

// Rotate moves the rotation so that the next draw is the address at index i,
// counting from 0 in file order. Out of range values wrap.
//
// [New] picks a random starting point, which is right for a long-running
// process and wrong for a test that wants to know what comes next. This is the
// way to pin it. It is also how to carry a position across a restart, if the
// caller keeps one somewhere.
func (p *Pool) Rotate(i int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.entries) == 0 {
		p.idx = 0
		return
	}
	p.idx = ((i % len(p.entries)) + len(p.entries)) % len(p.entries)
}

// Block sidelines a proxy for the given duration after it was throttled.
//
// A shorter Block never shortens a cooldown already in place. Two failures on
// one address should not leave it less penalised than one.
func (p *Pool) Block(pr Proxy, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	e := p.find(pr)
	if e == nil {
		return
	}

	// Counted even when the cooldown is not extended. Two failures on one
	// address are two failures, and that count is the point of the record.
	e.blocked++

	if until := time.Now().Add(d); e.blockedUntil.Before(until) {
		e.blockedUntil = until
	}
}

// find resolves a Proxy to its entry.
//
// The index is checked first and only trusted when the URL at that position
// still matches, because [Pool.Remove] and [Pool.Replace] can shift an index a
// caller is holding. Matching by URL is the fallback, and also what makes a
// Proxy built by hand work.
func (p *Pool) find(pr Proxy) *entry {
	if pr.Index >= 0 && pr.Index < len(p.entries) && p.entries[pr.Index].url == pr.URL {
		return p.entries[pr.Index]
	}
	for _, e := range p.entries {
		if e.url == pr.URL {
			return e
		}
	}
	return nil
}

// --- changing the list ------------------------------------------------------

// Add appends addresses, normalising each one the way [Load] does, and reports
// how many were new.
//
// Addresses already in the pool are skipped rather than appended twice. A
// duplicate is not harmless: round robin would hand it out twice per cycle, so
// that address would quietly take a double share of the traffic and reach a
// rate limit first.
//
// If any address cannot be parsed, nothing is added and the error names it.
// Adding nine of ten silently is the failure mode [Load] already refuses.
func (p *Pool) Add(raw ...string) (int, error) {
	urls, err := normalizeAll(raw)
	if err != nil {
		return 0, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	have := make(map[string]bool, len(p.entries))
	for _, e := range p.entries {
		have[e.url] = true
	}

	added := 0
	for _, u := range urls {
		if have[u] {
			continue
		}
		have[u] = true
		p.entries = append(p.entries, &entry{url: u})
		added++
	}
	return added, nil
}

// Remove drops an address and reports whether it was there. It can be passed in
// any format [Normalize] accepts.
//
// It also matches the string verbatim, and that is not belt and braces. [New]
// takes its addresses as given, so a pool can hold something Normalize would
// reject, and matching only the normalised form would leave that entry in the
// pool with no way to take it out.
//
// Removing the last entry leaves an empty pool, and [Pool.Next] then reports
// unhealthy rather than panicking.
func (p *Pool) Remove(raw string) bool {
	verbatim := strings.TrimSpace(raw)
	normalised, err := Normalize(raw)

	p.mu.Lock()
	defer p.mu.Unlock()

	for i, e := range p.entries {
		if e.url == verbatim || (err == nil && e.url == normalised) {
			p.entries = append(p.entries[:i], p.entries[i+1:]...)
			return true
		}
	}
	return false
}

// Replace swaps the whole list, keeping the cooldown of every address that
// survives the swap.
//
// This is what a provider handing out a fresh list every so often needs.
// Building a second Pool would do the swap too, and would forget that four of
// those addresses were blocked a minute ago: they would go straight back into
// rotation, be refused again, and the pool would relearn the same thing every
// time the list refreshed.
//
// If any address cannot be parsed, the pool is left untouched.
func (p *Pool) Replace(raw []string) error {
	urls, err := normalizeAll(raw)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	cooldown := make(map[string]time.Time, len(p.entries))
	for _, e := range p.entries {
		cooldown[e.url] = e.blockedUntil
	}

	entries := make([]*entry, 0, len(urls))
	seen := make(map[string]bool, len(urls))
	for _, u := range urls {
		if seen[u] {
			continue
		}
		seen[u] = true
		entries = append(entries, &entry{url: u, blockedUntil: cooldown[u]})
	}

	p.entries = entries

	// The rotation position is kept, not reset.
	//
	// Resetting it sends the next draw back to the head of the list, and
	// Replace is built for a provider that hands out a fresh list on a timer,
	// so it happens over and over in one process. Measured on a pool of 1000
	// refreshed every 30 minutes with 100 requests in between: addresses 0 to
	// 99 carried every request of the day and the other 900 carried none.
	//
	// It is a position in the rotation and not an identity, so it does not
	// matter that the address it points at may have changed. It only has to
	// stay in range.
	if len(p.entries) > 0 {
		p.idx %= len(p.entries)
	} else {
		p.idx = 0
	}
	return nil
}

// normalizeAll converts every entry or returns the first error, so that a
// caller either gets the whole list or none of it.
func normalizeAll(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	for i, r := range raw {
		u, err := Normalize(r)
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		out = append(out, u)
	}
	return out, nil
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

// Stats is what one address has done. It is a copy, so it can be held, sorted
// and logged without touching the pool again.
type Stats struct {
	Proxy Proxy

	// HandedOut counts every time [Pool.Next] returned this address, including
	// the times it came back with healthy false. The caller receives it either
	// way, and leaving those out would understate the address carrying an
	// exhausted pool.
	HandedOut int

	// Blocked counts every [Pool.Block] that resolved to this address, whether
	// or not it extended a cooldown already in place.
	Blocked int

	// LastUsed is when Next last returned it. Zero means never, which is worth
	// looking for: an address that has never been handed out on a pool that has
	// been running for hours is usually a sign the rotation is not reaching it.
	LastUsed time.Time

	// CoolingUntil is when the current cooldown expires. Zero means healthy.
	CoolingUntil time.Time
}

func (s Stats) String() string {
	out := fmt.Sprintf("%s  out=%d blocked=%d", s.Proxy, s.HandedOut, s.Blocked)
	if s.LastUsed.IsZero() {
		out += " never used"
	}
	if d := time.Until(s.CoolingUntil); d > 0 {
		out += fmt.Sprintf(" cooling %s", d.Round(time.Second))
	}
	return out
}

// Stats returns a snapshot per address, in file order.
//
// Two counts and a total is not enough to tune a pool. Providers differ
// enormously in how often their addresses get refused, so a pool mixing two of
// them looks fine in aggregate while half of it fails. This is the per-address
// view that shows which half.
func (p *Pool) Stats() []Stats {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	out := make([]Stats, 0, len(p.entries))
	for i, e := range p.entries {
		s := Stats{
			Proxy:     Proxy{URL: e.url, Index: i},
			HandedOut: e.handedOut,
			Blocked:   e.blocked,
			LastUsed:  e.lastUsed,
		}
		// An expired cooldown is reported as healthy rather than as a past
		// timestamp, so a caller does not have to compare against the clock.
		if e.blockedUntil.After(now) {
			s.CoolingUntil = e.blockedUntil
		}
		out = append(out, s)
	}
	return out
}

// All returns every proxy in the pool, in file order, regardless of cooldown.
//
// Use it when you want the list rather than the picker, for instance to pin
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
