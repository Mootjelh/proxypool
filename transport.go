package proxypool

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// DefaultCooldown is how long [Transport] sidelines an address when no
// Cooldown is set.
const DefaultCooldown = 5 * time.Minute

// ErrPoolEmpty is returned by [Transport.RoundTrip] when the pool holds no
// addresses at all. Every address cooling down is not this error: see
// [Transport.RoundTrip].
var ErrPoolEmpty = errors.New("proxypool: pool is empty")

// Transport is an [http.RoundTripper] that takes an address from a [Pool] for
// each request and blocks that address when the request goes wrong.
//
// It exists because the loop around [Pool.Next] and [Pool.Block] is the same
// everywhere: pick, build a client, send, judge the result, report back. Put it
// behind a RoundTripper and the rotation becomes one line of client setup:
//
//	client := &http.Client{Transport: &proxypool.Transport{Pool: pool}}
//
// The zero value is not usable; Pool is required. Safe for concurrent use.
//
// It does not retry. A request that fails comes back to the caller as it is,
// with the address already sidelined, so the next attempt lands somewhere else.
// Retrying here would mean guessing how many attempts the caller wants and
// whether the request body can be replayed, and neither is knowable from
// inside a RoundTripper.
type Transport struct {
	// Pool is where addresses come from. Required.
	Pool *Pool

	// BlockOn decides whether a finished request counts against the address it
	// went through. Nil means [DefaultBlockOn].
	//
	// It is called once per request with exactly what RoundTrip is about to
	// return, so one of resp and err is always nil.
	//
	// It must not read or close resp.Body. That body is the caller's, and a
	// hook that reads it hands them an empty one.
	BlockOn func(resp *http.Response, err error) bool

	// Cooldown is how long a failing address is sidelined. Zero means
	// [DefaultCooldown].
	Cooldown time.Duration

	// Base builds the RoundTripper for one address. Nil means
	// [Proxy.Transport], a plain *http.Transport.
	//
	// This is the seam for a caller who brings their own stack, which is a
	// common reason to be running proxies in the first place: a browser-grade
	// TLS client, a SOCKS dialer, an instrumented transport. It is called once
	// per address and the result is kept.
	Base func(Proxy) (http.RoundTripper, error)

	mu sync.Mutex
	rt map[string]http.RoundTripper
}

// RoundTrip sends one request through the next address in the pool.
//
// When every address is cooling down it sends anyway, through the one
// recovering soonest, which is what [Pool.Next] hands back. The alternatives
// are worse: sleeping would spend a latency budget the caller never agreed to,
// and failing would turn a pool that is merely busy into an outage. The caller
// still finds out, because that request is the one most likely to be refused,
// and [Pool.Stats] shows the cooldowns.
//
// An empty pool is different and returns [ErrPoolEmpty]. There is nothing to
// send through and no amount of waiting produces one.
//
// http.Client calls RoundTrip once per hop, so a redirect chain is spread over
// several addresses. That is usually what a rotating pool is for. If a
// redirect has to stay on one address, follow it yourself.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.Pool == nil {
		return nil, errors.New("proxypool: Transport.Pool is nil")
	}

	p, healthy := t.Pool.Next()
	if !healthy && p.Index < 0 {
		return nil, ErrPoolEmpty
	}

	rt, err := t.roundTripperFor(p)
	if err != nil {
		return nil, err
	}

	resp, err := rt.RoundTrip(req)

	blockOn := t.BlockOn
	if blockOn == nil {
		blockOn = DefaultBlockOn
	}
	if blockOn(resp, err) {
		cooldown := t.Cooldown
		if cooldown <= 0 {
			cooldown = DefaultCooldown
		}
		t.Pool.Block(p, cooldown)
	}

	return resp, err
}

// roundTripperFor returns the RoundTripper for one address, building it the
// first time.
//
// One per address, kept for the life of the Transport. Building a fresh
// *http.Transport per request would throw away the connection pool with it, so
// every request would pay a new TCP and TLS handshake through the proxy, which
// is the slowest part of the whole exchange.
func (t *Transport) roundTripperFor(p Proxy) (http.RoundTripper, error) {
	t.mu.Lock()
	rt, ok := t.rt[p.URL]
	t.mu.Unlock()
	if ok {
		return rt, nil
	}

	// Built outside the lock. Base is the caller's code and may be slow, and
	// holding the mutex across it would stall every other request in the
	// process. Two goroutines racing on the same new address both build one and
	// the loser's copy is dropped, which costs nothing: it has not opened a
	// connection yet.
	build := t.Base
	if build == nil {
		build = func(p Proxy) (http.RoundTripper, error) { return p.Transport() }
	}
	built, err := build(p)
	if err != nil {
		return nil, err
	}
	if built == nil {
		return nil, fmt.Errorf("proxypool: Base returned a nil RoundTripper for %s", p)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if rt, ok := t.rt[p.URL]; ok {
		return rt, nil
	}
	if t.rt == nil {
		t.rt = make(map[string]http.RoundTripper)
	}
	t.rt[p.URL] = built
	return built, nil
}

// CloseIdleConnections closes idle connections on every address's transport,
// for the ones that support it. http.Client calls this on its own
// CloseIdleConnections.
func (t *Transport) CloseIdleConnections() {
	t.mu.Lock()
	rts := make([]http.RoundTripper, 0, len(t.rt))
	for _, rt := range t.rt {
		rts = append(rts, rt)
	}
	t.mu.Unlock()

	for _, rt := range rts {
		if c, ok := rt.(interface{ CloseIdleConnections() }); ok {
			c.CloseIdleConnections()
		}
	}
}

// DefaultBlockOn is what [Transport] uses when BlockOn is nil. It blocks the
// address on 403, on 429, and on any transport error, timeouts included.
//
// What it deliberately leaves alone:
//
//   - 404 and the rest of 4xx. The origin answered a question about a URL. The
//     address delivered the request perfectly well.
//   - 5xx. An origin having a bad minute would otherwise sideline the entire
//     pool within seconds, and then there is nothing left to retry on when the
//     origin comes back.
//   - a request the caller cancelled. Nothing is known about the address, so
//     penalising it would let an aborted request poison a good one.
//
// A deadline the caller set on the request cannot be told apart here from an
// address being too slow to answer, and it is counted as a failure. That is the
// intended way round: on a rotating pool, slow is a kind of broken, and the
// cooldown expires on its own.
func DefaultBlockOn(resp *http.Response, err error) bool {
	if err != nil {
		return !errors.Is(err, context.Canceled)
	}
	if resp == nil {
		return false
	}
	switch resp.StatusCode {
	case http.StatusForbidden, http.StatusTooManyRequests:
		return true
	}
	return false
}
