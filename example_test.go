package proxypool_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/Mootjelh/proxypool"
)

// The usual loop: take an address, use it, sideline it when the far end
// complains, and let it come back on its own.
//
// Note what the log lines look like. All three entries share one host:port and
// differ only in the username, which is how sticky residential pools work, so
// the index is the only thing that tells them apart once the credentials are
// stripped.
func Example() {
	pool := proxypool.New([]string{
		"http://sess-a:pw@gw.example.com:5555",
		"http://sess-b:pw@gw.example.com:5555",
		"http://sess-c:pw@gw.example.com:5555",
	})

	for i := 0; i < 4; i++ {
		p, healthy := pool.Next()
		if !healthy {
			fmt.Println("every address is cooling down; soonest back:", p)
			break
		}
		fmt.Println("request", i, "via", p)

		if i == 1 {
			// Suppose that one came back 429.
			pool.Block(p, time.Hour)
		}
	}

	fmt.Printf("healthy: %d of %d\n", pool.Healthy(), pool.Len())

	// Output:
	// request 0 via pool#0 gw.example.com:5555
	// request 1 via pool#1 gw.example.com:5555
	// request 2 via pool#2 gw.example.com:5555
	// request 3 via pool#0 gw.example.com:5555
	// healthy: 2 of 3
}

// Normalize accepts the formats providers actually hand out, none of which is
// the URL an HTTP client wants.
func ExampleNormalize() {
	for _, raw := range []string{
		"1.2.3.4:8080",
		"gw.example.com:7000:user:pass",
		"user:pass@1.2.3.4:8080",
		"socks5://user:pass@host:1080",
	} {
		u, err := proxypool.Normalize(raw)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Println(u)
	}

	// Output:
	// http://1.2.3.4:8080
	// http://user:pass@gw.example.com:7000
	// http://user:pass@1.2.3.4:8080
	// socks5://user:pass@host:1080
}

// Transport is the bridge to net/http. Build a fresh client per address rather
// than swapping the proxy on a shared transport: a transport pools connections,
// and a pooled connection is already through the old proxy.
func ExampleProxy_Transport() {
	pool := proxypool.New([]string{"http://user:pw@gw.example.com:5555"})

	p, _ := pool.Next()
	transport, err := p.Transport()
	if err != nil {
		fmt.Println(err)
		return
	}

	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	_ = client

	fmt.Println("routing through", p)

	// Output:
	// routing through pool#0 gw.example.com:5555
}

// A Transport puts the rotation behind net/http, so the rest of the program
// never mentions proxies again. Each request takes the next address, and one
// that answers 403 or 429, or cannot be reached at all, is sidelined.
func ExampleTransport() {
	// Stands in for a proxy whose address the target has had enough of.
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer refusing.Close()

	pool := proxypool.New([]string{refusing.URL})
	client := &http.Client{
		Transport: &proxypool.Transport{Pool: pool, Cooldown: 10 * time.Minute},
		Timeout:   20 * time.Second,
	}

	resp, err := client.Get("http://example.invalid/")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	resp.Body.Close()

	fmt.Println("status", resp.StatusCode)
	fmt.Printf("healthy: %d of %d\n", pool.Healthy(), pool.Len())

	// Output:
	// status 429
	// healthy: 0 of 1
}

// BlockOn decides what counts against an address. Wrap DefaultBlockOn rather
// than replacing it when the target has a signal of its own on top of the
// usual ones.
func ExampleTransport_blockOn() {
	// This one answers 503 when it does not want an address, which the default
	// leaves alone because a 503 is normally the origin having a bad minute.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	pool := proxypool.New([]string{server.URL})
	client := &http.Client{
		Transport: &proxypool.Transport{
			Pool: pool,
			BlockOn: func(resp *http.Response, err error) bool {
				if proxypool.DefaultBlockOn(resp, err) {
					return true
				}
				return resp != nil && resp.StatusCode == http.StatusServiceUnavailable
			},
			Cooldown: 10 * time.Minute,
		},
		Timeout: 20 * time.Second,
	}

	resp, err := client.Get("http://example.invalid/")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	resp.Body.Close()

	fmt.Printf("healthy: %d of %d\n", pool.Healthy(), pool.Len())

	// Output:
	// healthy: 0 of 1
}
