// Package webfetch is a faithful, dependency-light Go port of the reference
// Python "mcp-server-fetch" tool (github.com/modelcontextprotocol/servers,
// src/fetch). It fetches a URL, optionally extracts the
// page's main content as Markdown, honours robots.txt for autonomous fetches,
// and returns text ready to hand to an LLM.
//
// The observable contract of the upstream tool is reproduced exactly: the
// autonomous User-Agent string, robots.txt handling (including its HTTP
// status-code branches), the HTML/raw content-type heuristic, the "Contents of
// <url>:" wrapper, and the truncation / error strings.
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
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/temoto/robotstxt"
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
	// IgnoreRobots skips the robots.txt check. The reference container does not
	// set this, so it defaults to false (robots honoured).
	IgnoreRobots bool
}

// Fetch reproduces the upstream fetch tool's call_tool: it honours robots.txt
// (unless IgnoreRobots), fetches the URL, extracts/keeps the content, applies
// start_index/max_length paging, and returns the text wrapped exactly as
// upstream does ("<prefix>Contents of <url>:\n<content>").
//
// It returns a non-nil error on the same conditions upstream raises McpError
// (robots denial, connection failure, HTTP status >= 400). Callers that have an
// alternate reader (e.g. a headless-browser fallback) should treat a non-nil
// error as "try the fallback".
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

	if !opts.IgnoreRobots {
		if err := checkMayAutonomouslyFetchURL(ctx, rawURL, userAgent); err != nil {
			return "", err
		}
	}

	content, prefix, err := fetchURL(ctx, rawURL, userAgent, opts.Raw)
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

// robotsTxtURL reconstructs scheme://host/robots.txt for a URL.
func robotsTxtURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	robots := &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/robots.txt"}
	return robots.String(), nil
}

// checkMayAutonomouslyFetchURL enforces robots.txt for autonomous fetches,
// mirroring upstream's status-code handling and error messages.
func checkMayAutonomouslyFetchURL(ctx context.Context, rawURL, userAgent string) error {
	robotURL, err := robotsTxtURL(rawURL)
	if err != nil {
		return fmt.Errorf("Failed to fetch robots.txt %s due to a connection issue", robotURL)
	}

	// Upstream uses httpx's default 5s timeout for the robots.txt request.
	client := newHTTPClient(5 * time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotURL, nil)
	if err != nil {
		return fmt.Errorf("Failed to fetch robots.txt %s due to a connection issue", robotURL)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Failed to fetch robots.txt %s due to a connection issue", robotURL)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fmt.Errorf("When fetching robots.txt (%s), received status %d so assuming that autonomous fetching is not allowed, the user can try manually fetching by using the fetch prompt", robotURL, resp.StatusCode)
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return nil
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("Failed to fetch robots.txt %s due to a connection issue", robotURL)
	}
	// Strip comment lines before parsing, matching upstream.
	var kept []string
	for _, line := range strings.Split(string(bodyBytes), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	robots, err := robotstxt.FromBytes([]byte(strings.Join(kept, "\n")))
	if err != nil {
		// A malformed robots.txt is treated as permissive (Protego parses
		// leniently); do not block the fetch on a parse error.
		return nil
	}
	testPath := rawURL
	if u, perr := url.Parse(rawURL); perr == nil {
		if ru := u.RequestURI(); ru != "" {
			testPath = ru
		}
	}
	if !robots.TestAgent(testPath, userAgent) {
		return fmt.Errorf("The sites robots.txt (%s), specifies that autonomous fetching of this page is not allowed, "+
			"<useragent>%s</useragent>\n<url>%s</url><robots>\n%s\n</robots>\n"+
			"The assistant must let the user know that it failed to view the page. The assistant may provide further guidance based on the above information.\n"+
			"The assistant can tell the user that they can try manually fetching the page by using the fetch prompt within their UI.",
			robotURL, userAgent, rawURL, string(bodyBytes))
	}
	return nil
}

// fetchURL fetches the URL and returns (content, prefix). content is either
// extracted Markdown or the raw body; prefix is the non-empty note prepended
// for non-simplifiable content types, matching upstream.
func fetchURL(ctx context.Context, rawURL, userAgent string, forceRaw bool) (string, string, error) {
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

	contentType := resp.Header.Get("content-type")
	// Decode the body to UTF-8 using the content-type charset (and any HTML
	// meta charset), mirroring httpx's response.text behaviour.
	decoded, err := charset.NewReader(resp.Body, contentType)
	if err != nil {
		decoded = resp.Body
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

	if isPageHTML && !forceRaw {
		return extractContentFromHTML(pageRaw, rawURL), "", nil
	}
	return pageRaw, fmt.Sprintf("Content type %s cannot be simplified to markdown, but here is the raw content:\n", contentType), nil
}

// extractContentFromHTML extracts the main article content and converts it to
// Markdown, mirroring upstream's readabilipy + markdownify(ATX). On extraction
// failure it returns the same error sentinel upstream returns.
func extractContentFromHTML(html, rawURL string) string {
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
	// Markdownify-matching options: ATX headings, "*" bullets and "*" emphasis
	// delimiter (upstream markdownify's defaults), so simple pages render
	// identically to the Python tool.
	converter := md.NewConverter("", true, &md.Options{
		HeadingStyle:     "atx",
		BulletListMarker: "*",
		EmDelimiter:      "*",
	})
	markdown, err := converter.ConvertString(cleaned.String())
	if err != nil || strings.TrimSpace(markdown) == "" {
		return "<error>Page failed to be simplified from HTML</error>"
	}
	return markdown
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
