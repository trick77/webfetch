# webfetch

A dependency-light Go port of the reference Python
[`mcp-server-fetch`](https://github.com/modelcontextprotocol/servers/tree/main/src/fetch)
tool.

It fetches a URL, extracts the main content as Markdown (or returns raw HTML),
and returns text ready to hand to an LLM — all **in-process**, with no Node
subprocess and no sidecar container.

```go
out, err := webfetch.Fetch(ctx, "https://example.com/article", webfetch.Options{
    MaxLength:  5000, // default when 0
    StartIndex: 0,
    Raw:        false,
})
```

## Tool

The module exposes a single tool, equivalent to upstream `mcp-server-fetch`:

**`fetch`** — Fetches a URL from the internet and extracts its contents as
markdown.

| Parameter     | Type    | Required | Default | Description                                        |
| ------------- | ------- | -------- | ------- | -------------------------------------------------- |
| `url`         | string  | yes      | —       | URL to fetch                                       |
| `max_length`  | integer | no       | `5000`  | Maximum number of characters to return             |
| `start_index` | integer | no       | `0`     | Start content from this character index            |
| `raw`         | boolean | no       | `false` | Get raw content without markdown conversion        |

(`webfetch.Fetch` maps these to the `Options` fields `MaxLength`, `StartIndex`,
and `Raw`.)

## Fidelity

The observable contract of the upstream tool is reproduced closely: the
autonomous `User-Agent`, the HTML/raw content-type heuristic, the
`Contents of <url>:` wrapper, and the truncation / error strings.

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

### Difference from upstream: robots.txt

Unlike `mcp-server-fetch`, this module **does not enforce `robots.txt`** — it
fetches the requested URL directly. This is a deliberate policy choice.

## SSRF protection: only public IPs are reachable

By default the fetcher can reach **only globally-routable public unicast
addresses**. This directly addresses the standard fetch-server caveat that such
a tool "can access local/internal IP addresses and may represent a security
risk": here it cannot.

Enforcement is in the dialer (`net.Dialer.Control`), so it runs **after DNS
resolution** against the concrete IP the socket will connect to — which also
covers HTTP redirects (every hop is re-dialed and re-checked) and DNS-rebinding
(the resolved IP is what's validated, not the hostname). It is a strict
default-deny allowlist; the following are refused:

- loopback (`127.0.0.0/8`, `::1`) and unspecified (`0.0.0.0`, `::`)
- private (`10/8`, `172.16/12`, `192.168/16`) and IPv6 ULA (`fc00::/7`)
- carrier-grade NAT (`100.64.0.0/10`)
- link-local, including the cloud metadata endpoint `169.254.169.254`
- broadcast, multicast, and IANA special-use / documentation / benchmarking
  ranges
- IPv4-mapped IPv6 forms of any of the above (normalized before checking)
