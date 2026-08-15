package proxypool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{
			name: "host:port:user:pass, the residential provider default",
			raw:  "gw.example.com:7000:user123:pass456",
			want: "http://user123:pass456@gw.example.com:7000",
		},
		{
			name: "host:port only",
			raw:  "1.2.3.4:8080",
			want: "http://1.2.3.4:8080",
		},
		{
			name: "user:pass@host:port",
			raw:  "user:pass@1.2.3.4:8080",
			want: "http://user:pass@1.2.3.4:8080",
		},
		{
			name: "already an http url",
			raw:  "http://user:pass@host:8080",
			want: "http://user:pass@host:8080",
		},
		{
			name: "socks5 url is preserved",
			raw:  "socks5://user:pass@host:1080",
			want: "socks5://user:pass@host:1080",
		},
		{
			name: "surrounding whitespace is trimmed",
			raw:  "  1.2.3.4:8080  ",
			want: "http://1.2.3.4:8080",
		},
		{
			name: "a reserved character in the password is escaped",
			raw:  "gw.example.com:7000:user:p@ss",
			want: "http://user:p%40ss@gw.example.com:7000",
		},
		{
			name:    "empty",
			raw:     "   ",
			wantErr: true,
		},
		{
			name:    "three parts is ambiguous",
			raw:     "host:port:user",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Normalize(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("Normalize(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestRoundRobin(t *testing.T) {
	p := New([]string{"a", "b", "c"})

	var got []string
	for i := 0; i < 6; i++ {
		pr, healthy := p.Next()
		if !healthy {
			t.Fatalf("iteration %d: expected a healthy proxy", i)
		}
		got = append(got, pr.URL)
	}

	want := []string{"a", "b", "c", "a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rotation = %v, want %v", got, want)
		}
	}
}

func TestIndexIsStable(t *testing.T) {
	p := New([]string{"a", "b", "c"})

	for i := 0; i < 6; i++ {
		pr, _ := p.Next()
		want := i % 3
		if pr.Index != want {
			t.Errorf("iteration %d: Index = %d, want %d", i, pr.Index, want)
		}
	}
}

func TestSkipsBlocked(t *testing.T) {
	p := New([]string{"a", "b", "c"})
	p.Block(Proxy{URL: "b", Index: 1}, time.Minute)

	if p.Healthy() != 2 {
		t.Fatalf("Healthy() = %d, want 2", p.Healthy())
	}
	for i := 0; i < 10; i++ {
		pr, healthy := p.Next()
		if !healthy {
			t.Fatalf("iteration %d: expected a healthy proxy", i)
		}
		if pr.URL == "b" {
			t.Fatalf("iteration %d: handed out blocked proxy b", i)
		}
	}
}

func TestAllBlockedReportsUnhealthy(t *testing.T) {
	p := New([]string{"a", "b"})
	p.Block(Proxy{URL: "a", Index: 0}, 2*time.Minute)
	p.Block(Proxy{URL: "b", Index: 1}, time.Minute)

	if p.Healthy() != 0 {
		t.Fatalf("Healthy() = %d, want 0", p.Healthy())
	}

	pr, healthy := p.Next()
	if healthy {
		t.Fatal("expected healthy=false when every proxy is cooling down")
	}
	if pr.URL != "b" {
		t.Errorf("Next() = %q, want %q (the one recovering soonest)", pr.URL, "b")
	}
}

func TestBlockDoesNotShortenCooldown(t *testing.T) {
	p := New([]string{"a"})
	pr := Proxy{URL: "a", Index: 0}

	p.Block(pr, time.Hour)
	p.Block(pr, time.Second)

	if _, healthy := p.Next(); healthy {
		t.Error("a short Block should not clear a longer existing cooldown")
	}
}

func TestRecoversAfterCooldown(t *testing.T) {
	p := New([]string{"a"})
	p.Block(Proxy{URL: "a", Index: 0}, 20*time.Millisecond)

	if _, healthy := p.Next(); healthy {
		t.Fatal("expected the proxy to be blocked immediately after Block")
	}
	time.Sleep(40 * time.Millisecond)
	if _, healthy := p.Next(); !healthy {
		t.Error("expected the proxy to recover once the cooldown elapsed")
	}
}

func TestBlockByURLWhenIndexIsUnknown(t *testing.T) {
	p := New([]string{"a", "b"})

	// A Proxy built by hand, with no meaningful index.
	p.Block(Proxy{URL: "b", Index: -1}, time.Minute)

	if p.Healthy() != 1 {
		t.Errorf("Healthy() = %d, want 1", p.Healthy())
	}
}

func TestEmptyPoolMeansDirect(t *testing.T) {
	p := New(nil)
	if p.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", p.Len())
	}
	pr, healthy := p.Next()
	if !pr.IsDirect() || !healthy {
		t.Errorf("Next() = (%q, %v), want a healthy direct entry", pr.URL, healthy)
	}
	if got := pr.String(); got != "direct" {
		t.Errorf("String() = %q, want %q", got, "direct")
	}
}

func TestMaskRemovesCredentials(t *testing.T) {
	tests := []struct{ in, want string }{
		{"http://user:pass@gw.example.com:7000", "gw.example.com:7000"},
		{"socks5://u:p@1.2.3.4:1080", "1.2.3.4:1080"},
		{"http://1.2.3.4:8080", "1.2.3.4:8080"},
		{"", "direct"},
	}
	for _, tt := range tests {
		if got := Mask(tt.in); got != tt.want {
			t.Errorf("Mask(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestStringDisambiguatesSharedHosts is the reason Proxy carries an index.
//
// A rotating residential pool typically gives every entry the same host:port
// and varies only the username, which encodes the sticky session. Masking the
// credentials, which is mandatory, makes them all print identically, so a log
// showing a working rotation is indistinguishable from one showing a pool stuck
// on a single address.
func TestStringDisambiguatesSharedHosts(t *testing.T) {
	p := New([]string{
		"http://sess-1:pw@gw.example.com:5555",
		"http://sess-2:pw@gw.example.com:5555",
	})

	first, _ := p.Next()
	second, _ := p.Next()

	if first.String() == second.String() {
		t.Fatalf("both entries log as %q; they are indistinguishable", first.String())
	}
	for _, pr := range []Proxy{first, second} {
		if strings.Contains(pr.String(), "pw") {
			t.Errorf("String() leaked credentials: %q", pr.String())
		}
	}
}

func TestTransport(t *testing.T) {
	direct, err := Proxy{}.Transport()
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	if direct.Proxy != nil {
		t.Error("a direct entry should produce a transport with no proxy")
	}

	via, err := Proxy{URL: "http://u:p@1.2.3.4:8080"}.Transport()
	if err != nil {
		t.Fatalf("proxied: %v", err)
	}
	if via.Proxy == nil {
		t.Error("a proxied entry should set Transport.Proxy")
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxies.txt")
	body := "# a comment\n\n1.2.3.4:8080\ngw.example.com:7000:user:pass\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Len() != 2 {
		t.Fatalf("Len() = %d, want 2 (comments and blank lines skipped)", p.Len())
	}
}

// TestLoadStripsBOM covers the failure this package exists to have already
// solved: a proxy list written by PowerShell carries a UTF-8 BOM, which glues
// three invisible bytes to the first hostname only.
func TestLoadStripsBOM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bom.txt")
	if err := os.WriteFile(path, []byte("\ufeff1.2.3.4:8080\n5.6.7.8:8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := p.URLs()[0]
	if got != "http://1.2.3.4:8080" {
		t.Errorf("first entry = %q, want the BOM stripped", got)
	}
}

func TestLoadRejectsABadLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.txt")
	if err := os.WriteFile(path, []byte("1.2.3.4:8080\nnonsense:a:b\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error naming the bad line")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error = %q, want it to name line 2", err)
	}
}

func TestConcurrentNextAndBlock(t *testing.T) {
	p := New([]string{"a", "b", "c", "d"})

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 200; j++ {
				pr, _ := p.Next()
				p.Block(pr, time.Millisecond)
				_ = p.Healthy()
				_ = pr.String()
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// --- changing the list -------------------------------------------------------

func TestAdd(t *testing.T) {
	p := New([]string{"http://1.2.3.4:8080"})

	n, err := p.Add("5.6.7.8:9090", "gw.example.com:7000:user:pass")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if n != 2 {
		t.Errorf("Add returned %d, want 2", n)
	}
	if p.Len() != 3 {
		t.Errorf("Len() = %d, want 3", p.Len())
	}

	urls := p.URLs()
	if urls[1] != "http://5.6.7.8:9090" {
		t.Errorf("added entry = %q, want it normalised", urls[1])
	}
}

// A duplicate would take a double share of the traffic, because round robin
// hands it out once per copy.
func TestAddSkipsDuplicates(t *testing.T) {
	p := New([]string{"http://1.2.3.4:8080"})

	n, err := p.Add("1.2.3.4:8080", "http://1.2.3.4:8080", "5.6.7.8:9090")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if n != 1 {
		t.Errorf("Add returned %d, want 1 (two of three were already present)", n)
	}
	if p.Len() != 2 {
		t.Errorf("Len() = %d, want 2", p.Len())
	}
}

func TestAddRejectsTheWholeBatch(t *testing.T) {
	p := New([]string{"http://1.2.3.4:8080"})

	if _, err := p.Add("5.6.7.8:9090", "nonsense:a:b"); err == nil {
		t.Fatal("expected an error naming the bad entry")
	}
	if p.Len() != 1 {
		t.Errorf("Len() = %d, want 1: a batch with a bad entry must add nothing", p.Len())
	}
}

func TestRemove(t *testing.T) {
	p := New([]string{"http://1.2.3.4:8080", "http://5.6.7.8:9090"})

	// Any format Normalize accepts resolves to the same address.
	if !p.Remove("1.2.3.4:8080") {
		t.Error("Remove reported the address was absent")
	}
	if p.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", p.Len())
	}
	if p.Remove("9.9.9.9:1") {
		t.Error("Remove reported an address that was never there")
	}
}

func TestRemovingTheLastEntryDoesNotPanic(t *testing.T) {
	p := New([]string{"http://1.2.3.4:8080"})

	if !p.Remove("1.2.3.4:8080") {
		t.Fatal("Remove failed")
	}
	if p.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", p.Len())
	}

	pr, healthy := p.Next()
	if healthy {
		t.Error("an empty pool must report unhealthy")
	}
	if pr.Index != -1 {
		t.Errorf("Index = %d, want -1 to mark an entry that came from nowhere", pr.Index)
	}
	if p.Healthy() != 0 {
		t.Errorf("Healthy() = %d, want 0", p.Healthy())
	}
}

// Block resolves a Proxy by index first. Once entries can be removed, an index
// a caller is still holding can point at a different address, and blocking the
// wrong one is silent: the intended address keeps being handed out and the
// innocent one goes quiet.
func TestAHeldProxyStillBlocksItsOwnAddress(t *testing.T) {
	p := New([]string{"a", "b", "c"})

	p.Next()            // a
	held, _ := p.Next() // b, at index 1
	if held.URL != "b" || held.Index != 1 {
		t.Fatalf("fixture: held = %+v, want b at index 1", held)
	}

	// After this, index 1 is "c".
	p.Remove("a")

	p.Block(held, time.Hour)

	if p.Healthy() != 1 {
		t.Fatalf("Healthy() = %d, want 1", p.Healthy())
	}
	for i := 0; i < 5; i++ {
		pr, healthy := p.Next()
		if !healthy {
			t.Fatal("expected c to still be healthy")
		}
		if pr.URL != "c" {
			t.Fatalf("Next() = %q, want c: the wrong address was blocked", pr.URL)
		}
	}
}

func TestReplaceKeepsCooldowns(t *testing.T) {
	p := New([]string{"http://a:1", "http://b:1", "http://c:1"})

	pr, _ := p.Next() // a
	p.Block(pr, time.Hour)
	if p.Healthy() != 2 {
		t.Fatalf("Healthy() = %d, want 2 before the swap", p.Healthy())
	}

	// a survives the swap and must still be cooling down; d is new.
	if err := p.Replace([]string{"http://a:1", "http://c:1", "http://d:1"}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	if p.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", p.Len())
	}
	if p.Healthy() != 2 {
		t.Errorf("Healthy() = %d, want 2: a was blocked and survived the swap", p.Healthy())
	}
	for i := 0; i < 6; i++ {
		pr, healthy := p.Next()
		if healthy && pr.URL == "http://a:1" {
			t.Fatal("a came back into rotation despite its cooldown")
		}
	}
}

func TestReplaceDropsWhatIsGone(t *testing.T) {
	p := New([]string{"a", "b"})

	if err := p.Replace([]string{"http://c:1"}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if p.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", p.Len())
	}
	if got := p.URLs()[0]; got != "http://c:1" {
		t.Errorf("URLs()[0] = %q, want the new list", got)
	}
}

func TestReplaceRejectsBadInputWithoutTouchingThePool(t *testing.T) {
	p := New([]string{"http://a:1", "http://b:1"})

	if err := p.Replace([]string{"http://c:1", "nonsense:a:b"}); err == nil {
		t.Fatal("expected an error")
	}
	if p.Len() != 2 {
		t.Errorf("Len() = %d, want 2: a failed Replace must change nothing", p.Len())
	}
	if got := p.URLs()[0]; got != "http://a:1" {
		t.Errorf("URLs()[0] = %q, want the original list", got)
	}
}

func TestConcurrentMutation(t *testing.T) {
	p := New([]string{"http://a:1", "http://b:1", "http://c:1", "http://d:1"})

	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				pr, _ := p.Next()
				p.Block(pr, time.Millisecond)
				_, _ = p.Add("http://e:1", "http://f:1")
				p.Remove("http://e:1")
				_ = p.Replace([]string{"http://a:1", "http://b:1", "http://c:1", "http://d:1"})
				_ = p.Healthy()
				_ = p.Len()
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		<-done
	}
}

// New takes addresses as given, so a pool can hold a string Normalize would
// reject. Matching only the normalised form would strand it there.
func TestRemoveMatchesVerbatimEntries(t *testing.T) {
	p := New([]string{"a", "b"})

	if !p.Remove("a") {
		t.Fatal("Remove could not find an entry that New accepted verbatim")
	}
	if p.Len() != 1 {
		t.Errorf("Len() = %d, want 1", p.Len())
	}
}
