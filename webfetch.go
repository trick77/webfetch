// Package webfetch is a dependency-light Go port of the reference Python
// "mcp-server-fetch" tool (github.com/modelcontextprotocol/servers, src/fetch).
// It fetches a URL, optionally extracts the page's main content as Markdown, and
// returns text ready to hand to an LLM.
//
// The observable contract of the upstream tool is reproduced closely: the
// autonomous User-Agent string, the HTML/raw content-type heuristic, the
// "Contents of <url>:" wrapper, and the truncation / error strings.
//
// The one unavoidable deviation is content extraction: upstream runs Mozilla
// Readability.js in a Node subprocess (readabilipy use_readability=True) plus
// Python markdownify. That JS pipeline cannot be reproduced byte-for-byte in
// pure Go, so we use codeberg.org/readeck/go-readability (a maintained Go port
// of the same Readability.js) followed by JohannesKaufmann/html-to-markdown
// configured to match markdownify's defaults (ATX headings, "*" bullets, "*"
// emphasis). On typical pages this is byte-identical to the Python output; the
// only observed difference is readability's URL normalization (e.g. a trailing
// slash added to bare links). Staying in-process (no Node, no subprocess) is
// also what makes the sidecar container removable, which is the point of this
// package.
//
// Beyond upstream, Options offers opt-in extensions (IncludeMetadata,
// ExtractPDF, FullPage / Selector / ExcludeSelectors) that default to off, and
// one deliberate default: MaxBodyBytes caps response bodies at 10 MiB so a
// model-chosen URL cannot exhaust memory. Only bodies over the cap behave
// differently from upstream (they fail instead of being read whole).
package webfetch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	readability "codeberg.org/readeck/go-readability/v2"
	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/PuerkitoBio/goquery"
	"github.com/ledongthuc/pdf"
	"golang.org/x/net/html/charset"
)

// DefaultUserAgentAutonomous is the User-Agent sent for autonomous (tool-driven)
// fetches. It is copied verbatim from upstream mcp-server-fetch; the reference
// server presents this generic identity rather than the real client, and we
// preserve that behaviour intentionally.
const DefaultUserAgentAutonomous = "ModelContextProtocol/1.0 (Autonomous; +https://github.com/modelcontextprotocol/servers)"

// defaultMaxLength mirrors the upstream Fetch.max_length default.
const defaultMaxLength = 5000

// DefaultMaxBodyBytes is the response-body cap applied when Options.MaxBodyBytes
// is zero. Upstream reads bodies unbounded; the cap is a deliberate divergence
// that only affects bodies larger than this.
const DefaultMaxBodyBytes = 10 << 20

// fetchTimeout mirrors upstream's httpx timeout=30.
const fetchTimeout = 30 * time.Second

// maxRedirects mirrors httpx's default max_redirects (Go's default is 10).
const maxRedirects = 20

// Upstream sentinel strings, reproduced verbatim.
const (
	errNoMoreContent = "<error>No more content available.</error>"
	errSimplify      = "<error>Page failed to be simplified from HTML</error>"
	errNoSelector    = "<error>No content matched the selector.</error>"
)

// Options mirror the upstream tool's parameters.
type Options struct {
	// MaxLength is the maximum number of characters to return. Zero means the
	// upstream default (5000).
	MaxLength int
	// StartIndex returns output starting at this character index, for paging a
	// previously truncated fetch.
	StartIndex int
	// Raw returns the actual HTML without Markdown simplification.
	Raw bool
	// UserAgent overrides the autonomous User-Agent. Empty uses the default.
	UserAgent string
	// MaxBodyBytes caps the size of the response body. A body larger than the
	// cap is rejected with a "Failed to fetch" error before any decoding, so a
	// model-chosen URL cannot exhaust memory. Zero (the default) applies
	// DefaultMaxBodyBytes (10 MiB); a negative value disables the cap, which is
	// upstream's unbounded behaviour. Bodies under the cap are unaffected.
	MaxBodyBytes int64
	// IncludeMetadata, when true, prepends a small YAML frontmatter block
	// (title, author, published, site, language — non-empty fields only) ahead
	// of the extracted Markdown. It applies only to the HTML-simplification path
	// (not Raw and not non-HTML content). Default false, which keeps the output
	// byte-identical to upstream mcp-server-fetch.
	//
	// The frontmatter counts as part of the returned content, so StartIndex /
	// MaxLength page over it too; hold IncludeMetadata constant across paged
	// calls so a page-2 StartIndex stays aligned.
	IncludeMetadata bool
	// ExtractPDF, when true, extracts the text of PDF responses (detected by
	// content-type or the "%PDF-" magic bytes) instead of returning the raw
	// bytes behind the "cannot be simplified" note. Extraction is pure-Go (no
	// subprocess). Raw takes precedence: if Raw is set, the PDF is returned
	// unextracted. Default false, preserving the upstream raw-bytes behaviour.
	ExtractPDF bool
	// FullPage converts the entire page to Markdown, skipping the Readability
	// main-content extraction. Use it when Readability over-strips (docs pages,
	// tables, sidebars you actually want). Ignored when Selector is set, and when
	// Raw is set. IncludeMetadata is not applied on this path. Default false.
	FullPage bool
	// Selector, when set, converts only the element(s) matching this CSS selector
	// to Markdown, skipping Readability (an escape hatch for targeting a specific
	// region). Takes precedence over FullPage. Ignored when Raw is set.
	// IncludeMetadata is not applied on this path. If nothing matches, the content
	// is the "<error>No content matched the selector.</error>" sentinel (with a
	// nil error). Default "".
	Selector string
	// ExcludeSelectors removes element(s) matching these CSS selectors before
	// conversion. Unlike FullPage/Selector it composes with every non-raw mode,
	// including the default Readability path (e.g. strip a cookie banner, then
	// simplify). Empty (the default) leaves the input untouched, so output stays
	// byte-identical to upstream. Ignored when Raw is set.
	ExcludeSelectors []string
}

// Fetch fetches the URL, extracts/keeps the content, applies
// start_index/max_length paging, and returns the text wrapped as
// "<prefix>Contents of <url>:\n<content>". Outbound connections are restricted
// to public IPs by the SSRF guard in the dialer.
//
// It returns a non-nil error on connection failure, HTTP status >= 400, or a
// response body over MaxBodyBytes. Callers that have an alternate reader (e.g.
// a headless-browser fallback) should treat a non-nil error as "try the
// fallback".
func Fetch(ctx context.Context, rawURL string, opts Options) (string, error) {
	if strings.TrimSpace(rawURL) == "" {
		return "", fmt.Errorf("URL is required")
	}
	userAgent := opts.UserAgent
	if userAgent == "" {
		userAgent = DefaultUserAgentAutonomous
	}
	maxLength := opts.MaxLength
	if maxLength <= 0 {
		maxLength = defaultMaxLength
	}
	startIndex := opts.StartIndex
	if startIndex < 0 {
		startIndex = 0
	}
	// Resolved cap: 0 = default, negative = unlimited (passed on as 0).
	maxBody := opts.MaxBodyBytes
	switch {
	case maxBody == 0:
		maxBody = DefaultMaxBodyBytes
	case maxBody < 0:
		maxBody = 0
	}

	content, prefix, err := fetchURL(ctx, rawURL, userAgent, maxBody, opts)
	if err != nil {
		return "", err
	}

	// Character (code-point) indexing, matching Python str slicing. Upstream
	// checks "start_index >= len(content)" and then "not truncated_content";
	// with max_length > 0 the second is implied by the first, so one empty
	// check covers both.
	out, got, total := sliceRunes(content, startIndex, maxLength)
	if out == "" {
		out = errNoMoreContent
	} else if remaining := total - (startIndex + got); got == maxLength && remaining > 0 {
		out += fmt.Sprintf("\n\n<error>Content truncated. Call the fetch tool with a start_index of %d to get more content.</error>", startIndex+got)
	}
	return fmt.Sprintf("%sContents of %s:\n%s", prefix, rawURL, out), nil
}

// fetchURL fetches the URL and returns (content, prefix). content is either
// extracted Markdown or the raw body, always valid UTF-8; prefix is the
// non-empty note prepended for non-simplifiable content types, matching
// upstream. maxBody is the resolved body cap in bytes (0 = unlimited).
// The capitalized error strings below are upstream's, reproduced verbatim as
// part of this package's observable contract (see the package doc). ST1005 is
// suppressed per site rather than in .golangci.yaml, which stays identical
// across the repo family.
func fetchURL(ctx context.Context, rawURL, userAgent string, maxBody int64, opts Options) (string, string, error) {
	client := &http.Client{
		Timeout:       fetchTimeout,
		Transport:     sharedTransport(),
		CheckRedirect: checkRedirect,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("Failed to fetch %s: %w", rawURL, err) //nolint:staticcheck // ST1005: upstream contract
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("Failed to fetch %s: %w", rawURL, err) //nolint:staticcheck // ST1005: upstream contract
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("Failed to fetch %s - status code %d", rawURL, resp.StatusCode) //nolint:staticcheck // ST1005: upstream contract
	}

	bodyBytes, err := readBody(resp.Body, maxBody)
	if err != nil {
		return "", "", fmt.Errorf("Failed to fetch %s: %w", rawURL, err) //nolint:staticcheck // ST1005: upstream contract
	}
	contentType := resp.Header.Get("content-type")

	// PDF handling runs on the raw bytes, before charset decoding (which would
	// corrupt binary content). Raw takes precedence, matching the option's doc.
	if opts.ExtractPDF && !opts.Raw && isPDF(contentType, bodyBytes) {
		text, pdfErr := extractPDFText(bodyBytes)
		if pdfErr != nil {
			return "", "", fmt.Errorf("Failed to extract PDF %s: %w", rawURL, pdfErr) //nolint:staticcheck // ST1005: upstream contract
		}
		return text, "", nil
	}

	pageRaw, err := decodeBody(bodyBytes, contentType)
	if err != nil {
		return "", "", fmt.Errorf("Failed to fetch %s: %w", rawURL, err) //nolint:staticcheck // ST1005: upstream contract
	}

	// Upstream: '"<html" in page_raw[:100]' — a character slice, not bytes.
	isPageHTML := strings.Contains(firstRunes(pageRaw, 100), "<html") ||
		strings.Contains(contentType, "text/html") ||
		contentType == ""

	if isPageHTML && !opts.Raw {
		// Links resolve against the final URL (after redirects); the wrapper
		// keeps the requested URL, as upstream does.
		return extractContentFromHTML(pageRaw, resp.Request.URL, opts), "", nil
	}
	return pageRaw, fmt.Sprintf("Content type %s cannot be simplified to markdown, but here is the raw content:\n", contentType), nil
}

// checkRedirect follows up to maxRedirects hops, matching httpx's default
// rather than net/http's 10.
func checkRedirect(_ *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	return nil
}

// readBody reads r in full. With limit > 0 a body longer than limit bytes is
// rejected (after reading at most limit+1 bytes) rather than buffered whole.
func readBody(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return b, nil
}

// decodeBody converts the body to a valid UTF-8 string, mirroring httpx's
// response.text: a charset declared in the Content-Type header (or a BOM, or an
// HTML meta charset) is honoured; otherwise the body is taken as UTF-8.
//
// charset.DetermineEncoding's own last resort is windows-1252, which would turn
// a UTF-8 body into mojibake whenever the first KiB happens to be pure ASCII.
// So an uncertain guess is only followed when the body is not valid UTF-8 (a
// real legacy-encoded page). Valid UTF-8 also skips the decoder copy entirely.
func decodeBody(body []byte, contentType string) (string, error) {
	enc, name, certain := charset.DetermineEncoding(body, contentType)
	if utf8.Valid(body) && (name == "utf-8" || !certain) {
		return string(body), nil
	}
	decoded, err := enc.NewDecoder().Bytes(body)
	if err != nil {
		return "", err
	}
	// Decoders substitute U+FFFD for undecodable input; enforce the invariant
	// regardless, since sliceRunes relies on it.
	return strings.ToValidUTF8(string(decoded), "\uFFFD"), nil
}

// firstRunes returns the first n runes of s (all of s if shorter).
func firstRunes(s string, n int) string {
	for off := range s {
		if n == 0 {
			return s[:off]
		}
		n--
	}
	return s
}

// sliceRunes returns s[start:start+n] in rune (code-point) terms, the number of
// runes in that slice, and the total rune count of s. For valid UTF-8 (which
// fetchURL guarantees) this equals string([]rune(s)[start:start+n]) without
// materialising a 4-byte-per-rune copy of the whole content. Requires n > 0.
func sliceRunes(s string, start, n int) (sub string, got, total int) {
	from, to := -1, -1
	for off := range s {
		switch total {
		case start:
			from = off
		case start + n:
			to = off
		}
		total++
	}
	if from < 0 {
		return "", 0, total
	}
	if to < 0 {
		return s[from:], total - start, total
	}
	return s[from:to], n, total
}

// extractContentFromHTML extracts the main article content and converts it to
// Markdown, mirroring upstream's readabilipy + markdownify(ATX). On extraction
// failure it returns the same error sentinel upstream returns. base (may be
// nil) is the final page URL, used to absolutize links.
func extractContentFromHTML(page string, base *url.URL, opts Options) string {
	var (
		article readability.Article
		err     error
	)
	// Escape-hatch pre-pass. ExcludeSelectors composes with every mode (including
	// the default Readability path); Selector / FullPage skip Readability. When
	// none are set this block is skipped entirely and the output is byte-identical
	// to upstream.
	if len(opts.ExcludeSelectors) > 0 || opts.Selector != "" || opts.FullPage {
		doc, perr := goquery.NewDocumentFromReader(strings.NewReader(page))
		if perr != nil {
			return errSimplify
		}
		for _, sel := range opts.ExcludeSelectors {
			if sel = strings.TrimSpace(sel); sel != "" {
				doc.Find(sel).Remove()
			}
		}
		if opts.Selector != "" || opts.FullPage {
			// Readability absolutizes links itself; do the same here so both
			// paths agree (scheme, host, and directory of the page).
			absolutizeLinks(doc, base)
			return selectorMarkdown(doc, opts)
		}
		// ExcludeSelectors only: hand the pruned tree straight to Readability.
		article, err = readability.FromDocument(doc.Nodes[0], base)
	} else {
		article, err = readability.FromReader(strings.NewReader(page), base)
	}
	if err != nil || article.Node == nil {
		return errSimplify
	}
	var cleaned strings.Builder
	if renderErr := article.RenderHTML(&cleaned); renderErr != nil || strings.TrimSpace(cleaned.String()) == "" {
		return errSimplify
	}
	markdown, err := convertHTMLToMarkdown(cleaned.String())
	if err != nil || strings.TrimSpace(markdown) == "" {
		return errSimplify
	}
	if opts.IncludeMetadata {
		if fm := articleFrontmatter(article); fm != "" {
			return fm + markdown
		}
	}
	return markdown
}

// selectorMarkdown converts a subtree (Selector) or the whole body (FullPage) of
// an already-pruned, link-absolutized document to Markdown, skipping
// Readability.
func selectorMarkdown(doc *goquery.Document, opts Options) string {
	var fragment string
	if opts.Selector != "" {
		sel := doc.Find(opts.Selector)
		if sel.Length() == 0 {
			return errNoSelector
		}
		var b strings.Builder
		sel.Each(func(_ int, s *goquery.Selection) {
			if h, err := goquery.OuterHtml(s); err == nil {
				b.WriteString(h)
			}
		})
		fragment = b.String()
	} else { // FullPage
		if body := doc.Find("body"); body.Length() > 0 {
			fragment, _ = body.Html()
		} else {
			fragment, _ = doc.Html()
		}
	}
	markdown, err := convertHTMLToMarkdown(fragment)
	if err != nil || strings.TrimSpace(markdown) == "" {
		return errSimplify
	}
	return markdown
}

// absolutizeLinks resolves every relative href/src in doc against base, in
// place. Absolute references (including data:, mailto: and javascript: URIs)
// and unparsable values are left untouched. A nil base is a no-op.
func absolutizeLinks(doc *goquery.Document, base *url.URL) {
	if base == nil {
		return
	}
	doc.Find("[href], [src]").Each(func(_ int, s *goquery.Selection) {
		for _, attr := range [...]string{"href", "src"} {
			raw, ok := s.Attr(attr)
			if !ok {
				continue
			}
			ref, err := url.Parse(strings.TrimSpace(raw))
			if err != nil || ref.IsAbs() {
				continue
			}
			s.SetAttr(attr, base.ResolveReference(ref).String())
		}
	})
}

// convertHTMLToMarkdown converts an HTML fragment with the markdownify-matching
// options (ATX headings, "*" bullets, "*" emphasis). Links are passed through
// untouched (empty domain): both callers absolutize them beforehand, and the
// converter's own domain handling would force an "http" scheme.
func convertHTMLToMarkdown(fragment string) (string, error) {
	converter := md.NewConverter("", true, &md.Options{
		HeadingStyle:     "atx",
		BulletListMarker: "*",
		EmDelimiter:      "*",
	})
	return converter.ConvertString(fragment)
}

// articleFrontmatter builds a small YAML frontmatter block from the metadata
// Readability already parsed (title, byline, published time, site name,
// language). Empty fields are omitted; if nothing is populated it returns "".
// Values are emitted as double-quoted YAML scalars so titles/bylines containing
// ":", "#", quotes, or a leading "-" cannot produce malformed frontmatter.
func articleFrontmatter(a readability.Article) string {
	var b strings.Builder
	add := func(key, val string) {
		if strings.TrimSpace(val) == "" {
			return
		}
		fmt.Fprintf(&b, "%s: %s\n", key, yamlQuote(val))
	}
	add("title", a.Title())
	add("author", a.Byline())
	if pt, err := a.PublishedTime(); err == nil && !pt.IsZero() {
		add("published", pt.Format(time.RFC3339))
	}
	add("site", a.SiteName())
	add("language", a.Language())
	if b.Len() == 0 {
		return ""
	}
	return "---\n" + b.String() + "---\n\n"
}

// yamlQuote renders s as a double-quoted YAML scalar, escaping backslashes and
// double quotes and flattening any embedded newlines/tabs to spaces so the
// value stays on a single frontmatter line.
func yamlQuote(s string) string {
	s = strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", " ",
		"\r", " ",
		"\t", " ",
	).Replace(s)
	return `"` + s + `"`
}

// isPDF reports whether the response is a PDF, by content-type or the "%PDF-"
// magic bytes (which also catches PDFs served as application/octet-stream).
func isPDF(contentType string, body []byte) bool {
	if strings.Contains(contentType, "application/pdf") ||
		strings.Contains(contentType, "application/x-pdf") {
		return true
	}
	return bytes.HasPrefix(body, []byte("%PDF-"))
}

// extractPDFText extracts the plain text of a PDF using a pure-Go parser. The
// parser can panic on malformed input, so a recover converts that into an error
// (callers with a headless-browser fallback treat a non-nil error as "try the
// fallback").
func extractPDFText(body []byte) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pdf parse panicked: %v", r)
		}
	}()
	reader, err := pdf.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", err
	}
	plain, err := reader.GetPlainText()
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if _, err := io.Copy(&sb, plain); err != nil {
		return "", err
	}
	return strings.ToValidUTF8(strings.TrimSpace(sb.String()), "\uFFFD"), nil
}

var (
	transportOnce sync.Once
	transport     *http.Transport
)

// sharedTransport returns the package-wide HTTP transport, built once. Sharing
// it across calls reuses connections and TLS sessions instead of stranding an
// idle keep-alive connection (and its goroutines) per fetch. Its dialer
// enforces the SSRF guard on every new connection; a pooled connection was
// validated when it was dialed. Redirects are followed by the client (like
// httpx follow_redirects=True) and each hop is re-dialed through the guard.
func sharedTransport() *http.Transport {
	transportOnce.Do(func() {
		transport = &http.Transport{
			// No proxy, deliberately: a proxy would move egress outside the
			// guarded dialer, which is where the SSRF check lives.
			Proxy:                 nil,
			DialContext:           dialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	})
	return transport
}
