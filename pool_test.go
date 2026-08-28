package proxypool

import (
	"fmt"
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

// The promise is the cycle, not where it starts. New picks a random starting
// address on purpose, so asserting that the first draw is "a" would be testing
// the offset instead of the rotation.
func TestRoundRobin(t *testing.T) {
	list := []string{"a", "b", "c"}
	p := New(list)

	var got []string
	for i := 0; i < 6; i++ {
		pr, healthy := p.Next()
		if !healthy {
			t.Fatalf("iteration %d: expected a healthy proxy", i)
		}
		got = append(got, pr.URL)
	}

	start := indexOf(list, got[0])
	if start < 0 {
		t.Fatalf("first draw %q is not in the list", got[0])
	}
	for i, u := range got {
		if want := list[(start+i)%len(list)]; u != want {
			t.Fatalf("rotation = %v, want it to cycle the list from %q", got, got[0])
		}
	}
}

// Pinning the start makes the sequence exact, which is what a caller needs when
// it wants to know what comes next.
func TestRotatePinsTheStartingPoint(t *testing.T) {
	p := New([]string{"a", "b", "c"})

	for _, start := range []int{0, 2, 1, 5, -1} {
		p.Rotate(start)
		pr, healthy := p.Next()
		if !healthy {
			t.Fatalf("Rotate(%d): expected a healthy proxy", start)
		}
		want := ((start % 3) + 3) % 3
		if pr.Index != want {
			t.Errorf("after Rotate(%d) the next draw was index %d, want %d", start, pr.Index, want)
		}
	}
}

// The starting point has to move between processes, or a restart draws the same
// addresses in the same order every time. Measured before this changed: 200
// restarts of a 1000-address pool taking 3 requests each used 3 addresses.
func TestNewDoesNotAlwaysStartAtTheSameAddress(t *testing.T) {
	const n = 50
	list := make([]string, n)
	for i := range list {
		list[i] = fmt.Sprintf("h%d:1", i)
	}

	seen := map[int]bool{}
	for restart := 0; restart < 200; restart++ {
		p := New(list)
		pr, _ := p.Next()
		seen[pr.Index] = true
	}

	// 200 starts over 50 addresses. Any fixed starting point gives 1. Asking
	// for a third of them is far enough below the expectation to not flake and
	// far enough above 1 to catch the bug.
	if len(seen) < n/3 {
		t.Errorf("200 fresh pools started at %d distinct addresses out of %d, want the starting point to move", len(seen), n)
	}
}

func indexOf(list []string, want string) int {
	for i, u := range list {
		if u == want {
			return i
		}
	}
	return -1
}

// Whatever the rotation hands out, the index has to match where that address
// sits in the list, or Stats cannot be read back against the file.
func TestIndexIsStable(t *testing.T) {
	list := []string{"a", "b", "c"}
	p := New(list)

	for i := 0; i < 6; i++ {
		pr, _ := p.Next()
		if pr.Index < 0 || pr.Index >= len(list) || list[pr.Index] != pr.URL {
			t.Errorf("draw %d: %q came back with index %d, which is not where it sits in %v", i, pr.URL, pr.Index, list)
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
	p.Rotate(0) // the fixture below names specific addresses

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
	p.Rotate(0) // so the draw below is a, and the comments mean what they say

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

// --- stats -------------------------------------------------------------------

func TestStatsCountsHandOutsAndBlocks(t *testing.T) {
	p := New([]string{"a", "b", "c"})

	p.Rotate(0)

	// a b c a: a goes out twice, b and c once each.
	for i := 0; i < 4; i++ {
		p.Next()
	}
	p.Block(Proxy{URL: "b", Index: 1}, time.Hour)
	p.Block(Proxy{URL: "b", Index: 1}, time.Second) // shorter, still a failure

	stats := p.Stats()
	if len(stats) != 3 {
		t.Fatalf("Stats() returned %d entries, want 3", len(stats))
	}

	byURL := map[string]Stats{}
	for _, s := range stats {
		byURL[s.Proxy.URL] = s
	}

	if got := byURL["a"].HandedOut; got != 2 {
		t.Errorf("a handed out %d times, want 2", got)
	}
	if got := byURL["c"].HandedOut; got != 1 {
		t.Errorf("c handed out %d times, want 1", got)
	}
	if got := byURL["b"].Blocked; got != 2 {
		t.Errorf("b blocked %d times, want 2: a shorter Block is still a failure", got)
	}
	if byURL["b"].CoolingUntil.IsZero() {
		t.Error("b should be cooling down")
	}
	if !byURL["a"].CoolingUntil.IsZero() {
		t.Error("a is healthy, CoolingUntil should be zero")
	}
	if byURL["a"].LastUsed.IsZero() {
		t.Error("a was handed out, LastUsed should be set")
	}
}

// An address that has never been handed out on a running pool usually means the
// rotation is not reaching it, so it has to be distinguishable from one that
// has.
func TestStatsShowsAnAddressThatWasNeverUsed(t *testing.T) {
	p := New([]string{"a", "b", "c"})
	p.Rotate(0)
	p.Next() // a only

	byURL := map[string]Stats{}
	for _, s := range p.Stats() {
		byURL[s.Proxy.URL] = s
	}

	if byURL["a"].LastUsed.IsZero() {
		t.Error("a was used, LastUsed should be set")
	}
	if !byURL["c"].LastUsed.IsZero() {
		t.Error("c was never used, LastUsed should be zero")
	}
	if got := byURL["c"].String(); !strings.Contains(got, "never used") {
		t.Errorf("String() = %q, want it to say never used", got)
	}
}

// An exhausted pool still hands out the address recovering soonest, and that is
// the one under the most load. Not counting it would hide exactly that.
func TestStatsCountsHandOutsFromAnExhaustedPool(t *testing.T) {
	p := New([]string{"a", "b"})
	p.Block(Proxy{URL: "a", Index: 0}, 2*time.Hour)
	p.Block(Proxy{URL: "b", Index: 1}, time.Hour)

	for i := 0; i < 3; i++ {
		if _, healthy := p.Next(); healthy {
			t.Fatal("expected an exhausted pool")
		}
	}

	for _, s := range p.Stats() {
		if s.Proxy.URL == "b" && s.HandedOut != 3 {
			t.Errorf("b handed out %d times, want 3", s.HandedOut)
		}
	}
}

// Stats must not leak credentials, for the same reason Proxy.String does not.
func TestStatsStringHidesCredentials(t *testing.T) {
	p := New([]string{"http://user:hunter2@gw.example.com:5555"})
	p.Next()

	got := p.Stats()[0].String()
	if strings.Contains(got, "hunter2") {
		t.Errorf("String() leaked the password: %q", got)
	}
	if !strings.Contains(got, "gw.example.com:5555") {
		t.Errorf("String() = %q, want the host in it", got)
	}
}

// Block resolves by URL when the index no longer matches. That path used to
// skip an entry whose cooldown was already longer, walking on to look at the
// rest of the pool instead of stopping at the address it had found.
func TestBlockByURLCountsEvenWhenTheCooldownStands(t *testing.T) {
	p := New([]string{"a", "b"})

	byHand := Proxy{URL: "b", Index: -1} // forces the URL path
	p.Block(byHand, time.Hour)
	p.Block(byHand, time.Second) // shorter, must not be dropped on the floor

	for _, s := range p.Stats() {
		if s.Proxy.URL == "b" && s.Blocked != 2 {
			t.Errorf("b blocked %d times, want 2", s.Blocked)
		}
	}
}

// Replace is for a provider that hands out a fresh list on a timer, so it runs
// again and again in one process. Resetting the rotation there sent every
// refresh back to the head of the list: measured on a pool of 1000 refreshed
// every 30 minutes with 100 draws in between, addresses 0 to 99 carried every
// request of the day and the other 900 carried none.
func TestReplaceKeepsTheRotationGoing(t *testing.T) {
	list := []string{"a:1", "b:1", "c:1", "d:1", "e:1", "f:1"}
	p := New(mustNormalize(t, list))
	p.Rotate(0)

	// Three draws, so the rotation sits at index 3.
	for i := 0; i < 3; i++ {
		p.Next()
	}
	if err := p.Replace(list); err != nil {
		t.Fatal(err)
	}

	pr, healthy := p.Next()
	if !healthy {
		t.Fatal("expected a healthy proxy")
	}
	if pr.URL != "http://d:1" {
		t.Errorf("after Replace the rotation handed out %q, want %q. Going back to the head is what leaves most of a pool unused", pr.URL, "http://d:1")
	}
}

// Over a day of refreshes the whole pool should be reached, not the first
// slice of it. This is the measurement from the issue, shrunk to a test.
func TestReplaceReachesTheWholePool(t *testing.T) {
	const n = 50
	list := make([]string, n)
	for i := range list {
		list[i] = fmt.Sprintf("h%d:1", i)
	}

	p := New(mustNormalize(t, list))
	seen := map[string]bool{}
	for refresh := 0; refresh < 12; refresh++ {
		for i := 0; i < 5; i++ { // fewer draws per period than the pool is long
			pr, _ := p.Next()
			seen[pr.URL] = true
		}
		if err := p.Replace(list); err != nil {
			t.Fatal(err)
		}
	}

	// 12 refreshes of 5 draws is 60 draws over 50 addresses, so every one
	// should have been handed out at least once.
	if len(seen) != n {
		t.Errorf("reached %d of %d addresses over 60 draws, want all of them", len(seen), n)
	}
}

// A shorter list must not leave the rotation pointing past the end.
func TestReplaceWithAShorterListStaysInRange(t *testing.T) {
	p := New(mustNormalize(t, []string{"a:1", "b:1", "c:1", "d:1", "e:1"}))
	for i := 0; i < 4; i++ {
		p.Next()
	}

	if err := p.Replace([]string{"x:1", "y:1"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		pr, healthy := p.Next()
		if !healthy {
			t.Fatalf("draw %d: expected a healthy proxy", i)
		}
		if pr.URL != "http://x:1" && pr.URL != "http://y:1" {
			t.Fatalf("draw %d handed out %q, which is not in the new list", i, pr.URL)
		}
	}
}

// mustNormalize puts a list through Normalize, so a pool built with New holds
// the same strings Replace would produce. Without it the two disagree and a
// surviving address looks like a new one.
func mustNormalize(t *testing.T, raw []string) []string {
	t.Helper()
	out := make([]string, len(raw))
	for i, r := range raw {
		u, err := Normalize(r)
		if err != nil {
			t.Fatalf("Normalize(%q): %v", r, err)
		}
		out[i] = u
	}
	return out
}
