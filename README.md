# webfetch

A dependency-light Go port of the reference Python
[`mcp-server-fetch`](https://github.com/modelcontextprotocol/servers/tree/main/src/fetch)
tool.

It fetches a URL, extracts the main content as Markdown (or returns raw HTML),
honours `robots.txt` for autonomous fetches, and returns text ready to hand to
an LLM — all **in-process**, with no Node subprocess and no sidecar container.

```go
out, err := webfetch.Fetch(ctx, "https://example.com/article", webfetch.Options{
    MaxLength:  5000, // default when 0
    StartIndex: 0,
    Raw:        false,
})
```

## Fidelity

The observable contract of the upstream tool is reproduced exactly: the
autonomous `User-Agent`, robots.txt handling (including its HTTP status-code
branches), the HTML/raw content-type heuristic, the `Contents of <url>:`
wrapper, and the truncation / error strings.

Content extraction is the one place byte-for-byte parity is impossible: upstream
runs Mozilla Readability.js in a Node subprocess (`readabilipy`) plus
`markdownify`. This module uses
[`codeberg.org/readeck/go-readability`](https://codeberg.org/readeck/go-readability)
(a maintained Go port of the same Readability.js) and
[`html-to-markdown`](https://github.com/JohannesKaufmann/html-to-markdown)
configured to match markdownify's defaults (ATX headings, `*` bullets, `*`
emphasis). On typical pages the output is byte-identical; the only observed
difference is readability's URL normalization (e.g. a trailing slash added to
bare links).

## SSRF protection

The upstream deployment isolated the fetch sidecar on its own Docker network so
model-chosen URLs could not reach internal hosts. Running in-process, that
protection is instead enforced in the dialer: connections to loopback, RFC1918,
link-local (including the `169.254.169.254` metadata endpoint), unspecified, and
multicast addresses are refused after DNS resolution.
