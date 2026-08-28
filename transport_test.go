package proxypool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubRT answers every request with a fixed status, or a fixed error.
type stubRT struct {
	proxy  Proxy
	status int
	err    error
}

func (s *stubRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{
		StatusCode: s.status,
		Body:       io.NopCloser(strings.NewReader("body of " + s.proxy.URL)),
		Request:    req,
	}, nil
}

// recorder builds a stub per address and remembers the order they were used in.
type recorder struct {
	status int
	err    error

	mu    sync.Mutex
	built int
	used  []string
}

func newRecorder(status int) *recorder {
	return &recorder{status: status}
}

func (r *recorder) base(p Proxy) (http.RoundTripper, error) {
	r.mu.Lock()
	r.built++
	r.mu.Unlock()
	return &tracking{r: r, url: p.URL, inner: &stubRT{proxy: p, status: r.status, err: r.err}}, nil
}

func (r *recorder) order() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.used...)
}

func (r *recorder) builds() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.built
}

type tracking struct {
	r     *recorder
	url   string
	inner http.RoundTripper
}

func (t *tracking) RoundTrip(req *http.Request) (*http.Response, error) {
	t.r.mu.Lock()
	t.r.used = append(t.r.used, t.url)
	t.r.mu.Unlock()
	return t.inner.RoundTrip(req)
}

func request(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest("GET", "http://example.test/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// send makes one request through tr and discards the body, for the tests whose
// subject is the rotation rather than the response.
func send(t *testing.T, tr *Transport) {
	t.Helper()
	resp, err := tr.RoundTrip(request(t))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
}

func TestTransportRotatesPerRequest(t *testing.T) {
	pool := New([]string{"http://a:1", "http://b:2", "http://c:3"})
	pool.Rotate(0) // New starts somewhere random, and this test wants an exact order
	rec := newRecorder(200)
	tr := &Transport{Pool: pool, Base: rec.base}

	for i := 0; i < 6; i++ {
		send(t, tr)
	}

	want := []string{"http://a:1", "http://b:2", "http://c:3", "http://a:1", "http://b:2", "http://c:3"}
	got := rec.order()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("addresses used\n got %v\nwant %v", got, want)
	}
}

func TestTransportBlocksTheAddressThatFailed(t *testing.T) {
	pool := New([]string{"http://a:1", "http://b:2"})
	pool.Rotate(0) // so the request below really does land on a
	rec := newRecorder(http.StatusForbidden)
	tr := &Transport{Pool: pool, Base: rec.base, Cooldown: time.Hour}

	// One request, which lands on a and is refused.
	send(t, tr)

	if got := pool.Healthy(); got != 1 {
		t.Fatalf("healthy after one 403 = %d, want 1", got)
	}
	stats := pool.Stats()
	if stats[0].Blocked != 1 {
		t.Errorf("a blocked = %d, want 1", stats[0].Blocked)
	}
	if stats[1].Blocked != 0 {
		t.Errorf("b blocked = %d, want 0, only a served a request", stats[1].Blocked)
	}
	if stats[0].CoolingUntil.IsZero() {
		t.Error("a is not cooling down")
	}
}

func TestTransportDoesNotBlockOnNotFound(t *testing.T) {
	pool := New([]string{"http://a:1", "http://b:2"})
	rec := newRecorder(http.StatusNotFound)
	tr := &Transport{Pool: pool, Base: rec.base}

	for i := 0; i < 4; i++ {
		send(t, tr)
	}

	if got := pool.Healthy(); got != 2 {
		t.Errorf("healthy after four 404s = %d, want 2. A 404 is the origin answering, not the address failing", got)
	}
}

func TestDefaultBlockOn(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name string
		resp *http.Response
		err  error
		want bool
	}{
		{"200", &http.Response{StatusCode: 200}, nil, false},
		{"403", &http.Response{StatusCode: 403}, nil, true},
		{"429", &http.Response{StatusCode: 429}, nil, true},
		{"404", &http.Response{StatusCode: 404}, nil, false},
		{"401", &http.Response{StatusCode: 401}, nil, false},
		{"503", &http.Response{StatusCode: 503}, nil, false},
		{"dial error", nil, errors.New("dial tcp: connection refused"), true},
		{"timeout", nil, context.DeadlineExceeded, true},
		{"wrapped timeout", nil, fmt.Errorf("proxyconnect: %w", context.DeadlineExceeded), true},
		{"cancelled", nil, cancelled.Err(), false},
		{"wrapped cancel", nil, fmt.Errorf("get: %w", context.Canceled), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DefaultBlockOn(c.resp, c.err); got != c.want {
				t.Errorf("DefaultBlockOn = %v, want %v", got, c.want)
			}
		})
	}
}

func TestTransportUsesTheBlockOnHook(t *testing.T) {
	pool := New([]string{"http://a:1", "http://b:2"})
	rec := newRecorder(http.StatusTeapot)

	var seen int
	tr := &Transport{
		Pool: pool,
		Base: rec.base,
		BlockOn: func(resp *http.Response, err error) bool {
			seen++
			return resp != nil && resp.StatusCode == http.StatusTeapot
		},
		Cooldown: time.Hour,
	}

	send(t, tr)

	if seen != 1 {
		t.Errorf("BlockOn called %d times, want 1", seen)
	}
	if got := pool.Healthy(); got != 1 {
		t.Errorf("healthy = %d, want 1. The hook asked for this address to be blocked", got)
	}
}

func TestTransportLeavesTheBodyForTheCaller(t *testing.T) {
	pool := New([]string{"http://a:1"})
	rec := newRecorder(200)
	tr := &Transport{Pool: pool, Base: rec.base}

	resp, err := tr.RoundTrip(request(t))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if want := "body of http://a:1"; string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

func TestTransportOnAnEmptyPool(t *testing.T) {
	pool := New([]string{"http://a:1"})
	if !pool.Remove("http://a:1") {
		t.Fatal("Remove did not find the only address")
	}

	tr := &Transport{Pool: pool, Base: newRecorder(200).base}
	if _, err := tr.RoundTrip(request(t)); !errors.Is(err, ErrPoolEmpty) {
		t.Errorf("err = %v, want ErrPoolEmpty", err)
	}
}

func TestTransportStillSendsWhenEveryAddressIsCoolingDown(t *testing.T) {
	pool := New([]string{"http://a:1", "http://b:2"})
	for _, p := range pool.All() {
		pool.Block(p, time.Hour)
	}
	if pool.Healthy() != 0 {
		t.Fatal("setup: expected every address to be cooling down")
	}

	rec := newRecorder(200)
	tr := &Transport{Pool: pool, Base: rec.base}

	resp, err := tr.RoundTrip(request(t))
	if err != nil {
		t.Fatalf("RoundTrip on an exhausted pool: %v, want the request to go out anyway", err)
	}
	resp.Body.Close()

	if len(rec.order()) != 1 {
		t.Errorf("requests sent = %d, want 1", len(rec.order()))
	}
}

func TestTransportBuildsOneRoundTripperPerAddress(t *testing.T) {
	pool := New([]string{"http://a:1", "http://b:2"})
	rec := newRecorder(200)
	tr := &Transport{Pool: pool, Base: rec.base}

	for i := 0; i < 10; i++ {
		send(t, tr)
	}

	if got := rec.builds(); got != 2 {
		t.Errorf("Base called %d times over 10 requests on 2 addresses, want 2. Rebuilding per request throws away the connection pool", got)
	}
}

func TestTransportReportsABaseThatFails(t *testing.T) {
	pool := New([]string{"http://a:1"})
	boom := errors.New("boom")
	tr := &Transport{
		Pool: pool,
		Base: func(Proxy) (http.RoundTripper, error) { return nil, boom },
	}

	if _, err := tr.RoundTrip(request(t)); !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap %v", err, boom)
	}
}

func TestTransportRejectsANilBaseResult(t *testing.T) {
	pool := New([]string{"http://a:1"})
	tr := &Transport{
		Pool: pool,
		Base: func(Proxy) (http.RoundTripper, error) { return nil, nil },
	}

	_, err := tr.RoundTrip(request(t))
	if err == nil || !strings.Contains(err.Error(), "nil RoundTripper") {
		t.Errorf("err = %v, want it to name the nil RoundTripper", err)
	}
}

func TestTransportWithoutAPool(t *testing.T) {
	tr := &Transport{}
	if _, err := tr.RoundTrip(request(t)); err == nil {
		t.Error("a Transport with no Pool sent a request")
	}
}

// TestTransportRoutesThroughTheAddress checks the default Base, the one nobody
// sets. Two httptest servers stand in for two proxies, and the request is
// judged by which one received it.
func TestTransportRoutesThroughTheAddress(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}

	// An HTTP proxy for a plain http:// URL is a server that receives the
	// request with an absolute URI. Answering it directly is enough here: the
	// question is which address net/http dialled, not what came back.
	newProxy := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits[name]++
			mu.Unlock()
			if !r.URL.IsAbs() {
				t.Errorf("%s got %q, want an absolute URI, so this was not a proxied request", name, r.URL)
			}
			w.WriteHeader(200)
		}))
	}
	one, two := newProxy("one"), newProxy("two")
	defer one.Close()
	defer two.Close()

	pool := New([]string{one.URL, two.URL})
	client := &http.Client{Transport: &Transport{Pool: pool}, Timeout: 10 * time.Second}

	for i := 0; i < 4; i++ {
		resp, err := client.Get("http://example.invalid/x")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	if hits["one"] != 2 || hits["two"] != 2 {
		t.Errorf("hits = %v, want two each. The rotation did not reach both addresses", hits)
	}
}

func TestTransportBlocksAnAddressThatCannotBeReached(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // nothing is listening there now

	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer live.Close()

	pool := New([]string{deadURL, live.URL})
	pool.Rotate(0) // so the first request is the one that goes to the dead address
	client := &http.Client{Transport: &Transport{Pool: pool, Cooldown: time.Hour}, Timeout: 10 * time.Second}

	// The first request goes to the dead address and fails.
	if _, err := client.Get("http://example.invalid/x"); err == nil {
		t.Fatal("request through a closed address succeeded")
	}
	if got := pool.Healthy(); got != 1 {
		t.Fatalf("healthy = %d, want 1. A transport error should sideline the address", got)
	}

	// The next one skips it, so it must succeed.
	resp, err := client.Get("http://example.invalid/x")
	if err != nil {
		t.Fatalf("second request: %v, want it routed past the blocked address", err)
	}
	resp.Body.Close()
}

func TestTransportDoesNotBlockACancelledRequest(t *testing.T) {
	pool := New([]string{"http://a:1", "http://b:2"})
	rec := newRecorder(0)
	rec.err = context.Canceled
	tr := &Transport{Pool: pool, Base: rec.base}

	if _, err := tr.RoundTrip(request(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := pool.Healthy(); got != 2 {
		t.Errorf("healthy = %d, want 2. The caller gave up, which says nothing about the address", got)
	}
}

func TestTransportCloseIdleConnections(t *testing.T) {
	// A real *http.Transport, so the assertion is that the call reaches
	// something that implements it rather than a stub recording it.
	pool := New([]string{"http://a:1"})
	tr := &Transport{Pool: pool}

	u, err := url.Parse("http://a:1")
	if err != nil {
		t.Fatal(err)
	}
	inner := &http.Transport{Proxy: http.ProxyURL(u)}
	tr.mu.Lock()
	tr.rt = map[string]http.RoundTripper{"http://a:1": inner}
	tr.mu.Unlock()

	tr.CloseIdleConnections() // must not panic, and must not deadlock on tr.mu
}

func TestTransportConcurrent(t *testing.T) {
	pool := New([]string{"http://a:1", "http://b:2", "http://c:3", "http://d:4"})
	rec := newRecorder(http.StatusTooManyRequests)
	tr := &Transport{Pool: pool, Base: rec.base, Cooldown: 20 * time.Millisecond}

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest("GET", "http://example.test/x", nil)
			if err != nil {
				return
			}
			resp, err := tr.RoundTrip(req)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()

	if len(rec.order()) != 40 {
		t.Errorf("requests sent = %d, want 40", len(rec.order()))
	}
}
