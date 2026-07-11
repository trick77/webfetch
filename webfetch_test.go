package webfetch

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

// allowLoopback relaxes the SSRF guard so tests can reach the loopback
// httptest server, then restores it. Production keeps the strict guard.
func allowLoopback(t *testing.T) {
	t.Helper()
	prev := dialControl
	dialControl = nil
	t.Cleanup(func() { dialControl = prev })
}

func TestFetch_HTMLToMarkdown(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>T</title></head><body>
			<article><h1>Hello World</h1><p>This is the main content of the article that readability should keep because it is long enough to be considered the primary body text of the page.</p></article>
			</body></html>`)
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/page", Options{})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.HasPrefix(out, fmt.Sprintf("Contents of %s/page:\n", srv.URL)) {
		t.Fatalf("missing wrapper prefix, got:\n%s", out)
	}
	if !strings.Contains(out, "# Hello World") {
		t.Fatalf("expected ATX heading in markdown, got:\n%s", out)
	}
	if !strings.Contains(out, "main content of the article") {
		t.Fatalf("expected body text, got:\n%s", out)
	}
}

func TestFetch_IncludeMetadata(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html lang="en"><head>
			<title>Great: Article #1 "quoted"</title>
			<meta name="author" content="Jane Doe">
			<meta property="article:published_time" content="2024-01-02T15:04:05Z">
			<meta property="og:site_name" content="Example Blog">
			</head><body>
			<article><h1>Great Article</h1><p>This is the main content of the article that readability should keep because it is long enough to be considered the primary body text of the page.</p></article>
			</body></html>`)
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/post", Options{IncludeMetadata: true})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	// Frontmatter sits inside the wrapper, ahead of the markdown.
	fmStart := fmt.Sprintf("Contents of %s/post:\n---\n", srv.URL)
	if !strings.HasPrefix(out, fmStart) {
		t.Fatalf("expected frontmatter after wrapper, got:\n%s", out)
	}
	for _, want := range []string{
		`title: "Great: Article #1 \"quoted\""`, // ":", "#" and quotes escaped/safe
		`author: "Jane Doe"`,
		`published: "2024-01-02T15:04:05Z"`,
		`site: "Example Blog"`,
		`language: "en"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected frontmatter line %q, got:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "\n---\n\nThis is the main content") {
		t.Fatalf("expected frontmatter to close before the markdown body, got:\n%s", out)
	}

	// Default (no IncludeMetadata) must not emit any frontmatter — fidelity.
	plain, err := Fetch(context.Background(), srv.URL+"/post", Options{})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if strings.Contains(plain, "---\n") || strings.Contains(plain, "title:") {
		t.Fatalf("default output must not contain frontmatter, got:\n%s", plain)
	}
}

func TestFetch_RawSkipsSimplification(t *testing.T) {
	allowLoopback(t)
	body := `<!doctype html><html><body><article><h1>Hi</h1><p>Body body body body body body body body body.</p></article></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/p", Options{Raw: true})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(out, "<article>") {
		t.Fatalf("raw mode should return unsimplified HTML, got:\n%s", out)
	}
}

func TestFetch_NonHTMLPrefix(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"key":"value"}`)
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/data.json", Options{})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(out, "Content type application/json cannot be simplified to markdown, but here is the raw content:") {
		t.Fatalf("expected non-simplifiable prefix, got:\n%s", out)
	}
	if !strings.Contains(out, `{"key":"value"}`) {
		t.Fatalf("expected raw json body, got:\n%s", out)
	}
}

func TestFetch_Truncation(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "ABCDEFGHIJ") // 10 chars
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/t", Options{MaxLength: 4, StartIndex: 0})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(out, "ABCD") {
		t.Fatalf("expected first 4 chars, got:\n%s", out)
	}
	if !strings.Contains(out, "start_index of 4") {
		t.Fatalf("expected truncation continuation note, got:\n%s", out)
	}

	// start past the end -> no more content
	out2, err := Fetch(context.Background(), srv.URL+"/t", Options{MaxLength: 4, StartIndex: 999})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(out2, "<error>No more content available.</error>") {
		t.Fatalf("expected no-more-content error, got:\n%s", out2)
	}
}

func TestFetch_NoRobotsEnforcement(t *testing.T) {
	allowLoopback(t)
	// robots.txt forbids everything; webfetch must ignore it entirely and never
	// even request /robots.txt.
	robotsRequested := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			robotsRequested = true
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello")
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/anything", Options{})
	if err != nil {
		t.Fatalf("Fetch must not enforce robots.txt, got: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("expected body, got:\n%s", out)
	}
	if robotsRequested {
		t.Fatal("webfetch must not request /robots.txt")
	}
}

func TestFetch_HTTPError(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL+"/boom", Options{})
	if err == nil || !strings.Contains(err.Error(), "status code 500") {
		t.Fatalf("expected status-code error, got: %v", err)
	}
}

func TestSSRF_GuardAllowsOnlyPublic(t *testing.T) {
	blocked := []string{
		"127.0.0.1:80",          // loopback
		"[::1]:80",              // loopback v6
		"10.1.2.3:443",          // RFC1918
		"192.168.1.1:80",        // RFC1918
		"172.16.0.1:80",         // RFC1918
		"100.64.0.1:80",         // CGNAT
		"169.254.169.254:80",    // link-local metadata endpoint
		"0.0.0.0:80",            // this-host
		"255.255.255.255:80",    // broadcast
		"224.0.0.1:80",          // multicast
		"198.18.0.1:80",         // benchmarking
		"192.0.2.1:80",          // TEST-NET-1
		"203.0.113.5:80",        // TEST-NET-3
		"[fc00::1]:80",          // IPv6 ULA
		"[fe80::1]:80",          // IPv6 link-local
		"[2001:db8::1]:80",      // IPv6 documentation
		"[::ffff:127.0.0.1]:80", // IPv4-mapped loopback (bypass attempt)
	}
	for _, addr := range blocked {
		if err := guardedControl("tcp", addr, nil); err == nil {
			t.Errorf("expected %s to be blocked", addr)
		}
	}
	allowed := []string{"1.1.1.1:443", "8.8.8.8:53", "93.184.216.34:80", "[2606:4700:4700::1111]:443"}
	for _, addr := range allowed {
		if err := guardedControl("tcp", addr, nil); err != nil {
			t.Errorf("expected %s to be allowed, got: %v", addr, err)
		}
	}
}

func TestFetch_SSRFBlocksLoopbackEndToEnd(t *testing.T) {
	// Guard active (not relaxed): a loopback target must be refused at dial.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "should never be reached")
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL+"/x", Options{})
	if err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("expected SSRF dial refusal, got: %v", err)
	}
}

// compile-time nod that guardedControl matches the Dialer.Control signature.
var _ func(string, string, syscall.RawConn) error = guardedControl
var _ = net.IPv4
