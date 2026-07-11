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
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
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

func TestFetch_RawSkipsSimplification(t *testing.T) {
	allowLoopback(t)
	body := `<!doctype html><html><body><article><h1>Hi</h1><p>Body body body body body body body body body.</p></article></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
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
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
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
	// 20 chars of plain text via a non-HTML type so content == body exactly.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
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

func TestFetch_RobotsDisallow(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "User-agent: *\nDisallow: /secret\n")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body><p>secret</p></body></html>")
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL+"/secret/page", Options{})
	if err == nil {
		t.Fatalf("expected robots.txt to block the fetch")
	}
	if !strings.Contains(err.Error(), "robots.txt") || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("unexpected robots error: %v", err)
	}

	// A path not disallowed should be allowed.
	if _, err := Fetch(context.Background(), srv.URL+"/public/page", Options{}); err != nil {
		t.Fatalf("public path should be allowed, got: %v", err)
	}
}

func TestFetch_Robots403BlocksAutonomous(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>x</body></html>")
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL+"/p", Options{})
	if err == nil || !strings.Contains(err.Error(), "status 403") {
		t.Fatalf("expected 403 robots block, got: %v", err)
	}
}

func TestFetch_IgnoreRobots(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello")
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/anything", Options{IgnoreRobots: true})
	if err != nil {
		t.Fatalf("IgnoreRobots should bypass robots.txt, got: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("expected body, got:\n%s", out)
	}
}

func TestFetch_HTTPError(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL+"/boom", Options{})
	if err == nil || !strings.Contains(err.Error(), "status code 500") {
		t.Fatalf("expected status-code error, got: %v", err)
	}
}

func TestSSRF_GuardRejectsPrivate(t *testing.T) {
	blocked := []string{
		"127.0.0.1:80", "[::1]:80", "10.1.2.3:443", "192.168.1.1:80",
		"172.16.0.1:80", "169.254.169.254:80", "0.0.0.0:80",
	}
	for _, addr := range blocked {
		if err := guardedControl("tcp", addr, nil); err == nil {
			t.Errorf("expected %s to be blocked", addr)
		}
	}
	allowed := []string{"1.1.1.1:443", "8.8.8.8:53", "93.184.216.34:80"}
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

	_, err := Fetch(context.Background(), srv.URL+"/x", Options{IgnoreRobots: true})
	if err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("expected SSRF dial refusal, got: %v", err)
	}
}

// compile-time nod that guardedControl matches the Dialer.Control signature.
var _ func(string, string, syscall.RawConn) error = guardedControl
var _ = net.IPv4
