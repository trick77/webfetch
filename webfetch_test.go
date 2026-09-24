package webfetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

// allowLoopback relaxes the SSRF guard so tests can reach the loopback
// httptest server, then restores it. Production keeps the strict guard.
func allowLoopback(t *testing.T) {
	t.Helper()
	prev := dialControl
	dialControl = nil
	t.Cleanup(func() {
		dialControl = prev
		// Drop pooled connections opened while the guard was relaxed so a
		// guard-on test can never ride one to loopback.
		sharedTransport().CloseIdleConnections()
	})
}

func TestFetch_HTMLToMarkdown(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

func TestFetch_ExtractPDF(t *testing.T) {
	allowLoopback(t)
	pdfBytes, err := os.ReadFile("testdata/sample.pdf")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(pdfBytes)
	}))
	defer srv.Close()

	// ExtractPDF=true -> the marker text embedded in the fixture is returned.
	out, err := Fetch(context.Background(), srv.URL+"/doc.pdf", Options{ExtractPDF: true})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.HasPrefix(out, fmt.Sprintf("Contents of %s/doc.pdf:\n", srv.URL)) {
		t.Fatalf("missing wrapper prefix, got:\n%s", out)
	}
	if !strings.Contains(out, "ZEBRA-42") {
		t.Fatalf("expected extracted PDF text, got:\n%s", out)
	}
	if strings.Contains(out, "cannot be simplified") {
		t.Fatalf("extracted PDF should not carry the raw-content note, got:\n%s", out)
	}

	// Default (ExtractPDF=false) -> upstream behaviour: raw bytes behind the note.
	plain, err := Fetch(context.Background(), srv.URL+"/doc.pdf", Options{})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(plain, "Content type application/pdf cannot be simplified to markdown") {
		t.Fatalf("default must keep the raw-content note for PDFs, got a different shape")
	}
	if strings.Contains(plain, "ZEBRA-42") {
		t.Fatalf("default must not extract PDF text")
	}
}

func TestFetch_EscapeHatch(t *testing.T) {
	allowLoopback(t)
	page := `<!doctype html><html lang="en"><body>
		<nav>NAVMARKER menu</nav>
		<div class="ad">ADMARKER buy now</div>
		<article id="main"><h1>Heading</h1><p>ARTICLEMARKER the primary body text, long enough to be the main content of the page.</p>
		<p class="drop">DROPMARKER a removable sentence with enough words that readability keeps it by default.</p></article>
		<footer>FOOTERMARKER copyright</footer>
		</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	// FullPage: skips Readability, so nav/footer that Readability drops survive.
	full, err := Fetch(context.Background(), srv.URL+"/p", Options{FullPage: true})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	for _, want := range []string{"NAVMARKER", "ARTICLEMARKER", "FOOTERMARKER"} {
		if !strings.Contains(full, want) {
			t.Fatalf("FullPage missing %q, got:\n%s", want, full)
		}
	}

	// Selector: only the matched subtree is converted.
	only, err := Fetch(context.Background(), srv.URL+"/p", Options{Selector: "#main"})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(only, "ARTICLEMARKER") {
		t.Fatalf("Selector should keep the target, got:\n%s", only)
	}
	if strings.Contains(only, "NAVMARKER") || strings.Contains(only, "FOOTERMARKER") {
		t.Fatalf("Selector should exclude non-target content, got:\n%s", only)
	}

	// ExcludeSelectors composes with FullPage: the ad is stripped, the rest stays.
	noAd, err := Fetch(context.Background(), srv.URL+"/p", Options{FullPage: true, ExcludeSelectors: []string{".ad"}})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if strings.Contains(noAd, "ADMARKER") {
		t.Fatalf("ExcludeSelectors should remove the ad, got:\n%s", noAd)
	}
	if !strings.Contains(noAd, "ARTICLEMARKER") {
		t.Fatalf("ExcludeSelectors removed too much, got:\n%s", noAd)
	}

	// ExcludeSelectors composes with the DEFAULT Readability path (no FullPage/
	// Selector): the pruned element is gone though Readability would keep it.
	def, err := Fetch(context.Background(), srv.URL+"/p", Options{})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(def, "DROPMARKER") {
		t.Fatalf("precondition: default Readability path should keep the drop paragraph, got:\n%s", def)
	}
	excl, err := Fetch(context.Background(), srv.URL+"/p", Options{ExcludeSelectors: []string{".drop"}})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if strings.Contains(excl, "DROPMARKER") {
		t.Fatalf("ExcludeSelectors should compose with the Readability path, got:\n%s", excl)
	}
	if !strings.Contains(excl, "ARTICLEMARKER") {
		t.Fatalf("ExcludeSelectors removed too much on the Readability path, got:\n%s", excl)
	}

	// No selector match: sentinel content, and importantly a nil error (so a
	// caller's fallback is not triggered by a mistyped selector).
	miss, err := Fetch(context.Background(), srv.URL+"/p", Options{Selector: "#does-not-exist"})
	if err != nil {
		t.Fatalf("no-match must not error (would misfire the fallback): %v", err)
	}
	if !strings.Contains(miss, "<error>No content matched the selector.</error>") {
		t.Fatalf("expected no-match sentinel, got:\n%s", miss)
	}
}

func TestFetch_RawSkipsSimplification(t *testing.T) {
	allowLoopback(t)
	body := `<!doctype html><html><body><article><h1>Hi</h1><p>Body body body body body body body body body.</p></article></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

// The error paths below were previously untested. They assert errors.Is
// unwrapping too: fetchURL wraps with %w, and a caller distinguishing a
// context cancellation from a genuine transport failure depends on that.

func TestFetch_InvalidURLRejected(t *testing.T) {
	allowLoopback(t)
	// A control character in the URL fails http.NewRequestWithContext before
	// any connection is attempted.
	_, err := Fetch(context.Background(), "http://exa\x7fmple.com", Options{})
	if err == nil {
		t.Fatal("expected an error for a malformed URL")
	}
	if !strings.Contains(err.Error(), "Failed to fetch") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestFetch_ConnectionRefused(t *testing.T) {
	allowLoopback(t)
	// Bind then close, so the port is almost certainly free and refuses.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if cerr := ln.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}

	_, err = Fetch(context.Background(), "http://"+addr, Options{})
	if err == nil {
		t.Fatal("expected an error when the connection is refused")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("transport error was not unwrappable, got: %v", err)
	}
}

func TestFetch_TruncatedBodyReportsReadFailure(t *testing.T) {
	allowLoopback(t)
	// Content-Length promises 1024 bytes, the handler writes 7 and returns, so
	// the connection closes short. client.Do has already succeeded by then
	// (headers and the first bytes are in the socket buffer), which puts the
	// failure in io.ReadAll deterministically rather than racing the dial.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "1024")
		if _, werr := w.Write([]byte("partial")); werr != nil {
			return
		}
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL, Options{})
	if err == nil {
		t.Fatal("expected an error when the body is truncated")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("body-read error was not unwrappable, got: %v", err)
	}
}

func TestFetch_CorruptPDFReportsExtractionFailure(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		// A valid PDF magic number followed by garbage: isPDF accepts it, the
		// extractor does not.
		fmt.Fprint(w, "%PDF-1.4\nnot actually a pdf body")
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), srv.URL, Options{ExtractPDF: true})
	if err == nil {
		t.Fatal("expected an error for an undecodable PDF")
	}
	if !strings.Contains(err.Error(), "Failed to extract PDF") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

// serve returns an httptest server that answers every request with the given
// content type and body.
func serve(t *testing.T, contentType string, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetch_UndeclaredCharsetIsUTF8(t *testing.T) {
	allowLoopback(t)
	// Pure ASCII for the first KiB (the charset sniffing window), then UTF-8.
	// Without a charset the old windows-1252 fallback turned "é" into "Ã©".
	body := append(bytes.Repeat([]byte("a"), 1200), "é ✓"...)
	for _, ct := range []string{"text/plain", "application/json", "text/html", ""} {
		srv := serve(t, ct, body)
		out, err := Fetch(context.Background(), srv.URL+"/x", Options{Raw: true, MaxLength: 2000})
		if err != nil {
			t.Fatalf("[%q] Fetch error: %v", ct, err)
		}
		if !strings.Contains(out, "é ✓") {
			t.Fatalf("[%q] expected UTF-8 to survive, got tail: %q", ct, out[len(out)-20:])
		}
	}
}

func TestFetch_DeclaredAndSniffedLegacyCharset(t *testing.T) {
	allowLoopback(t)
	latin1 := []byte("caf\xe9") // "café" in ISO-8859-1 / windows-1252
	cases := map[string]string{
		"text/plain; charset=iso-8859-1": "header-declared charset",
		"text/plain":                     "undeclared, not valid UTF-8: library guess (windows-1252)",
	}
	for ct, why := range cases {
		srv := serve(t, ct, latin1)
		out, err := Fetch(context.Background(), srv.URL+"/x", Options{})
		if err != nil {
			t.Fatalf("[%s] Fetch error: %v", why, err)
		}
		if !strings.Contains(out, "café") {
			t.Fatalf("[%s] expected transcoded text, got:\n%s", why, out)
		}
	}
	// A meta charset on an HTML page, no header charset.
	srv := serve(t, "text/html", []byte(`<html><head><meta charset="iso-8859-1"></head><body><p>caf`+"\xe9"+`</p></body></html>`))
	out, err := Fetch(context.Background(), srv.URL+"/x", Options{Raw: true})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.Contains(out, "café") {
		t.Fatalf("expected meta charset to be honoured, got:\n%s", out)
	}
}

func TestFetch_EmptyBody(t *testing.T) {
	allowLoopback(t)
	srv := serve(t, "text/plain", nil)
	out, err := Fetch(context.Background(), srv.URL+"/empty", Options{})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	want := "Content type text/plain cannot be simplified to markdown, but here is the raw content:\nContents of " + srv.URL + "/empty:\n<error>No more content available.</error>"
	if out != want {
		t.Fatalf("unexpected output for empty body:\n%s", out)
	}
}

func TestFetch_SelectorPathLinksResolveAgainstPage(t *testing.T) {
	allowLoopback(t)
	page := `<!doctype html><html><body><div id="c">
		<a href="/root">root</a>
		<a href="rel.html">rel</a>
		<a href="../up.html">up</a>
		<img src="//cdn.example/i.png" alt="i">
		<a href="mailto:a@example.com">mail</a>
		<a href="https://other.example/abs">abs</a>
		<a href="data:text/plain,hi">data</a>
		</div></body></html>`
	srv := serve(t, "text/html; charset=utf-8", []byte(page))

	for _, opts := range []Options{{Selector: "#c"}, {FullPage: true}} {
		out, err := Fetch(context.Background(), srv.URL+"/dir/sub/page", opts)
		if err != nil {
			t.Fatalf("Fetch error: %v", err)
		}
		for _, want := range []string{
			"(" + srv.URL + "/root)",
			"(" + srv.URL + "/dir/sub/rel.html)",
			"(" + srv.URL + "/dir/up.html)",
			"(http://cdn.example/i.png)", // protocol-relative takes the page scheme
			"(mailto:a@example.com)",
			"(https://other.example/abs)",
			"(data:text/plain,hi)",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("%+v: expected %q in:\n%s", opts, want, out)
			}
		}
	}
}

func TestAbsolutizeLinks_HTTPSBase(t *testing.T) {
	// The converter's own domain handling forced "http"; ours keeps the scheme.
	base, _ := url.Parse("https://example.com/a/b/page.html?q=1")
	doc, err := goqueryDoc(`<a href="/x">x</a><a href="y">y</a><img src="//h/i.png"><a href="#frag">f</a><a href="">e</a><a href="javascript:void(0)">j</a>`)
	if err != nil {
		t.Fatal(err)
	}
	absolutizeLinks(doc, base)
	got := []string{}
	doc.Find("[href], [src]").Each(func(_ int, s *goquery.Selection) {
		if v, ok := s.Attr("href"); ok {
			got = append(got, v)
		}
		if v, ok := s.Attr("src"); ok {
			got = append(got, v)
		}
	})
	want := []string{
		"https://example.com/x",
		"https://example.com/a/b/y",
		"https://h/i.png",
		"#frag", // in-page anchors stay relative, as on the Readability path
		"",
		"javascript:void(0)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// nil base: untouched.
	doc2, _ := goqueryDoc(`<a href="/x">x</a>`)
	absolutizeLinks(doc2, nil)
	if v, _ := doc2.Find("a").Attr("href"); v != "/x" {
		t.Fatalf("nil base must be a no-op, got %q", v)
	}
}

func goqueryDoc(fragment string) (*goquery.Document, error) {
	return goquery.NewDocumentFromReader(strings.NewReader(fragment))
}

func TestFetch_RedirectMovesLinkBaseNotWrapper(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			http.Redirect(w, r, "/new/dir/page", http.StatusFound)
		case "/new/dir/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<html><body><div id="c"><a href="rel.html">rel</a></div></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	out, err := Fetch(context.Background(), srv.URL+"/old", Options{Selector: "#c"})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if !strings.HasPrefix(out, "Contents of "+srv.URL+"/old:\n") {
		t.Fatalf("wrapper must keep the requested URL, got:\n%s", out)
	}
	if !strings.Contains(out, "("+srv.URL+"/new/dir/rel.html)") {
		t.Fatalf("relative link must resolve against the final URL, got:\n%s", out)
	}
}

func TestFetch_ExcludeSelectorsWithReadabilityBase(t *testing.T) {
	allowLoopback(t)
	// ExcludeSelectors-only path now hands the parsed tree to Readability
	// directly; links must still be absolutized via base as on the plain path.
	page := `<!doctype html><html><body><article><h1>T</h1>
		<p class="drop">DROPMARKER enough words here that readability keeps this paragraph around by default.</p>
		<p>KEEPMARKER the primary body text, long enough to be considered the main content of the page. See <a href="/link">the link</a> for more.</p>
		</article></body></html>`
	srv := serve(t, "text/html; charset=utf-8", []byte(page))
	out, err := Fetch(context.Background(), srv.URL+"/p", Options{ExcludeSelectors: []string{".drop"}})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if strings.Contains(out, "DROPMARKER") || !strings.Contains(out, "KEEPMARKER") {
		t.Fatalf("unexpected pruning result:\n%s", out)
	}
	if !strings.Contains(out, "("+srv.URL+"/link)") {
		t.Fatalf("expected absolutized link, got:\n%s", out)
	}
}

func TestSliceRunes_MatchesRuneSlicing(t *testing.T) {
	inputs := []string{"", "abc", "héllo wörld ✓✓✓", "日本語のテキスト", "a"}
	for _, s := range inputs {
		r := []rune(s)
		for start := 0; start <= len(r)+1; start++ {
			for n := 1; n <= len(r)+2; n++ {
				var want string
				if start < len(r) {
					end := min(start+n, len(r))
					want = string(r[start:end])
				}
				got, gotN, total := sliceRunes(s, start, n)
				if got != want || gotN != len([]rune(want)) || total != len(r) {
					t.Fatalf("sliceRunes(%q, %d, %d) = (%q, %d, %d), want (%q, %d, %d)",
						s, start, n, got, gotN, total, want, len([]rune(want)), len(r))
				}
			}
		}
	}
}

func TestFirstRunes(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"", 5, ""},
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc"},
		{"ééé<html", 3, "ééé"},
		{"ééé<html", 4, "ééé<"},
		{"abc", 0, ""},
	}
	for _, c := range cases {
		if got := firstRunes(c.s, c.n); got != c.want {
			t.Errorf("firstRunes(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
}

func TestFetch_HTMLSniffIsCharacterBased(t *testing.T) {
	allowLoopback(t)
	// 60 two-byte runes (120 bytes) precede "<html": within upstream's 100
	// *character* window, outside a 100 *byte* one. No content-type, so only the
	// sniff decides — but an empty content-type is itself an HTML signal
	// upstream, so use a non-HTML content type to isolate the sniff.
	body := strings.Repeat("é", 60) + "<html><body><article><p>SNIFFMARKER the main content of this page, long enough for readability to keep.</p></article></body></html>"
	srv := serve(t, "application/octet-stream", []byte(body))
	out, err := Fetch(context.Background(), srv.URL+"/x", Options{})
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if strings.Contains(out, "cannot be simplified") {
		t.Fatalf("expected the page to be sniffed as HTML, got:\n%s", out)
	}
	if !strings.Contains(out, "SNIFFMARKER") {
		t.Fatalf("expected extracted content, got:\n%s", out)
	}
}

func TestReadBody_Limit(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 100)
	if b, err := readBody(bytes.NewReader(body), 0); err != nil || len(b) != 100 {
		t.Fatalf("unlimited: got %d bytes, err %v", len(b), err)
	}
	if b, err := readBody(bytes.NewReader(body), 100); err != nil || len(b) != 100 {
		t.Fatalf("exactly at cap must pass: got %d bytes, err %v", len(b), err)
	}
	if _, err := readBody(bytes.NewReader(body), 99); err == nil || !strings.Contains(err.Error(), "exceeds 99 bytes") {
		t.Fatalf("one over cap must fail, got: %v", err)
	}
	// limit+1 must not overflow into a negative LimitReader size.
	if b, err := readBody(bytes.NewReader(body), math.MaxInt64); err != nil || len(b) != 100 {
		t.Fatalf("MaxInt64 must mean unlimited: got %d bytes, err %v", len(b), err)
	}
}

func TestFetch_MaxBodyBytes(t *testing.T) {
	allowLoopback(t)
	srv := serve(t, "text/plain", bytes.Repeat([]byte("x"), 100))

	_, err := Fetch(context.Background(), srv.URL+"/big", Options{MaxBodyBytes: 50})
	if err == nil || !strings.Contains(err.Error(), "Failed to fetch") || !strings.Contains(err.Error(), "exceeds 50 bytes") {
		t.Fatalf("expected body-cap error, got: %v", err)
	}
	for _, cap := range []int64{0, -1, 100} {
		if _, err := Fetch(context.Background(), srv.URL+"/big", Options{MaxBodyBytes: cap}); err != nil {
			t.Fatalf("MaxBodyBytes=%d should succeed for a 100-byte body: %v", cap, err)
		}
	}

	// A declared Content-Length over the cap is rejected before any body is
	// read at all.
	declared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1000))
	}))
	defer declared.Close()
	_, err = Fetch(context.Background(), declared.URL+"/declared", Options{MaxBodyBytes: 500})
	if err == nil || !strings.Contains(err.Error(), "exceeds 500 bytes") {
		t.Fatalf("expected Content-Length pre-check to reject, got: %v", err)
	}

	// The default cap is real: one byte over DefaultMaxBodyBytes fails, and the
	// handler is stopped early rather than streamed to completion. Chunked (no
	// Content-Length), so this exercises the read-side cap, not the pre-check.
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for i := 0; i < 10; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte("x"))
	}))
	defer huge.Close()
	_, err = Fetch(context.Background(), huge.URL+"/huge", Options{})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("exceeds %d bytes", DefaultMaxBodyBytes)) {
		t.Fatalf("expected default cap to trigger, got: %v", err)
	}
}

func TestFetch_RedirectLimit(t *testing.T) {
	allowLoopback(t)
	// /hop/<n>/<target> redirects to n+1 until n == target, then serves text.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n, target int
		if _, err := fmt.Sscanf(r.URL.Path, "/hop/%d/%d", &n, &target); err != nil {
			http.NotFound(w, r)
			return
		}
		if n < target {
			http.Redirect(w, r, fmt.Sprintf("/hop/%d/%d", n+1, target), http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "landed")
	}))
	defer srv.Close()

	// 15 hops exceeds net/http's default of 10; exactly 20 is httpx's limit
	// and must still be followed; 21 must not.
	for _, hops := range []int{15, maxRedirects} {
		out, err := Fetch(context.Background(), fmt.Sprintf("%s/hop/0/%d", srv.URL, hops), Options{})
		if err != nil || !strings.Contains(out, "landed") {
			t.Fatalf("%d redirects should be followed, got err=%v out=%q", hops, err, out)
		}
	}
	_, err := Fetch(context.Background(), fmt.Sprintf("%s/hop/0/%d", srv.URL, maxRedirects+1), Options{})
	if err == nil || !strings.Contains(err.Error(), "Failed to fetch") || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("%d redirects should fail, got: %v", maxRedirects+1, err)
	}
}

func TestFetch_ReusesConnections(t *testing.T) {
	allowLoopback(t)
	var newConns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hi")
	}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	for i := 0; i < 3; i++ {
		if _, err := Fetch(context.Background(), srv.URL+"/r", Options{}); err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
	}
	if got := newConns.Load(); got != 1 {
		t.Fatalf("expected one pooled connection across sequential fetches, server saw %d", got)
	}
}
