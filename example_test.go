package proxypool_test

import (
	"fmt"
	"net/http"
	"time"

	"github.com/Mootjelh/proxypool"
)

// The usual loop: take an address, use it, sideline it when the far end
// complains, and let it come back on its own.
//
// Note what the log lines look like. All three entries share one host:port and
// differ only in the username — which is how sticky residential pools work —
// so the index is the only thing that tells them apart once the credentials are
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
