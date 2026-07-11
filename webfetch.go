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
package webfetch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

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
// It returns a non-nil error on connection failure or HTTP status >= 400.
// Callers that have an alternate reader (e.g. a headless-browser fallback)
// should treat a non-nil error as "try the fallback".
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

	content, prefix, err := fetchURL(ctx, rawURL, userAgent, opts)
	if err != nil {
		return "", err
	}

	// Character (code-point) indexing, matching Python str slicing.
	runes := []rune(content)
	originalLength := len(runes)
	var out string
	if startIndex >= originalLength {
		out = "<error>No more content available.</error>"
	} else {
		end := startIndex + maxLength
		if end > originalLength {
			end = originalLength
		}
		truncated := string(runes[startIndex:end])
		if truncated == "" {
			out = "<error>No more content available.</error>"
		} else {
			out = truncated
			actualLen := end - startIndex
			remaining := originalLength - (startIndex + actualLen)
			if actualLen == maxLength && remaining > 0 {
				nextStart := startIndex + actualLen
				out += fmt.Sprintf("\n\n<error>Content truncated. Call the fetch tool with a start_index of %d to get more content.</error>", nextStart)
			}
		}
	}
	return fmt.Sprintf("%sContents of %s:\n%s", prefix, rawURL, out), nil
}

// fetchURL fetches the URL and returns (content, prefix). content is either
// extracted Markdown or the raw body; prefix is the non-empty note prepended
// for non-simplifiable content types, matching upstream.
func fetchURL(ctx context.Context, rawURL, userAgent string, opts Options) (string, string, error) {
	client := newHTTPClient(30 * time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("Failed to fetch %s: %v", rawURL, err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("Failed to fetch %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("Failed to fetch %s - status code %d", rawURL, resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("Failed to fetch %s: %v", rawURL, err)
	}
	contentType := resp.Header.Get("content-type")

	// PDF handling runs on the raw bytes, before charset decoding (which would
	// corrupt binary content). Raw takes precedence, matching the option's doc.
	if opts.ExtractPDF && !opts.Raw && isPDF(contentType, bodyBytes) {
		text, err := extractPDFText(bodyBytes)
		if err != nil {
			return "", "", fmt.Errorf("Failed to extract PDF %s: %v", rawURL, err)
		}
		return text, "", nil
	}

	// Decode the body to UTF-8 using the content-type charset (and any HTML
	// meta charset), mirroring httpx's response.text behaviour.
	decoded, err := charset.NewReader(bytes.NewReader(bodyBytes), contentType)
	if err != nil {
		decoded = bytes.NewReader(bodyBytes)
	}
	raw, err := io.ReadAll(decoded)
	if err != nil {
		return "", "", fmt.Errorf("Failed to fetch %s: %v", rawURL, err)
	}
	pageRaw := string(raw)

	head := pageRaw
	if len(head) > 100 {
		head = head[:100]
	}
	isPageHTML := strings.Contains(head, "<html") ||
		strings.Contains(contentType, "text/html") ||
		contentType == ""

	if isPageHTML && !opts.Raw {
		return extractContentFromHTML(pageRaw, rawURL, opts), "", nil
	}
	return pageRaw, fmt.Sprintf("Content type %s cannot be simplified to markdown, but here is the raw content:\n", contentType), nil
}

// extractContentFromHTML extracts the main article content and converts it to
// Markdown, mirroring upstream's readabilipy + markdownify(ATX). On extraction
// failure it returns the same error sentinel upstream returns.
func extractContentFromHTML(html, rawURL string, opts Options) string {
	// Escape-hatch pre-pass. ExcludeSelectors composes with every mode (including
	// the default Readability path); Selector / FullPage skip Readability. When
	// none are set this block is skipped entirely and the output is byte-identical
	// to upstream.
	if len(opts.ExcludeSelectors) > 0 || opts.Selector != "" || opts.FullPage {
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
		if err != nil {
			return "<error>Page failed to be simplified from HTML</error>"
		}
		for _, sel := range opts.ExcludeSelectors {
			if sel = strings.TrimSpace(sel); sel != "" {
				doc.Find(sel).Remove()
			}
		}
		if opts.Selector != "" || opts.FullPage {
			return selectorMarkdown(doc, rawURL, opts)
		}
		// ExcludeSelectors only: re-serialize the pruned document and fall through
		// to the normal Readability path below.
		if cleaned, err := doc.Html(); err == nil {
			html = cleaned
		}
	}

	var base *url.URL
	if u, err := url.Parse(rawURL); err == nil {
		base = u
	}
	article, err := readability.FromReader(strings.NewReader(html), base)
	if err != nil || article.Node == nil {
		return "<error>Page failed to be simplified from HTML</error>"
	}
	var cleaned strings.Builder
	if err := article.RenderHTML(&cleaned); err != nil || strings.TrimSpace(cleaned.String()) == "" {
		return "<error>Page failed to be simplified from HTML</error>"
	}
	// Empty domain keeps the converter behaviour byte-identical to upstream;
	// Readability already absolutized links via base.
	markdown, err := convertHTMLToMarkdown(cleaned.String(), "")
	if err != nil || strings.TrimSpace(markdown) == "" {
		return "<error>Page failed to be simplified from HTML</error>"
	}
	if opts.IncludeMetadata {
		if fm := articleFrontmatter(article); fm != "" {
			return fm + markdown
		}
	}
	return markdown
}

// selectorMarkdown converts a subtree (Selector) or the whole body (FullPage) of
// an already-pruned document to Markdown, skipping Readability. doc has already
// had ExcludeSelectors removed.
func selectorMarkdown(doc *goquery.Document, rawURL string, opts Options) string {
	var fragment string
	if opts.Selector != "" {
		sel := doc.Find(opts.Selector)
		if sel.Length() == 0 {
			return "<error>No content matched the selector.</error>"
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
	// Pass the host so relative links are absolutized, matching what the
	// Readability path gets from base.
	markdown, err := convertHTMLToMarkdown(fragment, domainOf(rawURL))
	if err != nil || strings.TrimSpace(markdown) == "" {
		return "<error>Page failed to be simplified from HTML</error>"
	}
	return markdown
}

// convertHTMLToMarkdown converts an HTML fragment with the markdownify-matching
// options (ATX headings, "*" bullets, "*" emphasis). domain, when non-empty,
// absolutizes root-relative links.
func convertHTMLToMarkdown(html, domain string) (string, error) {
	converter := md.NewConverter(domain, true, &md.Options{
		HeadingStyle:     "atx",
		BulletListMarker: "*",
		EmDelimiter:      "*",
	})
	return converter.ConvertString(html)
}

// domainOf returns the host of rawURL, or "" if it cannot be parsed.
func domainOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Host
	}
	return ""
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
	return strings.TrimSpace(sb.String()), nil
}

// newHTTPClient builds an HTTP client whose dialer enforces the SSRF guard and
// which follows redirects (like httpx follow_redirects=True).
func newHTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		DialContext:           newDialContext(),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}
