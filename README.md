# ddgo-mcp

A tiny [MCP](https://modelcontextprotocol.io/) server giving an LLM **web search
and page reading** — pure Go, single static binary, **no headless browser, no
Python**. Built to sit behind an MCP gateway (kgateway / agentgateway) as an SSE
backend, alongside other tool servers.

## Tools

| Tool | Args | Returns |
|------|------|---------|
| `search` | `query` (string, required), `max_results` (number, 1–10, default 5), `freshness` (`day`/`week`/`month`/`year`, optional) | Top results as title / url / snippet (scrapes DuckDuckGo HTML — no API key), ranked for `REGION` |
| `fetch`  | `url` (string, required) | Title, the page's published/updated dates when it declares them, and its main readable text ([go-readability](https://github.com/go-shiori/go-readability)); truncated to ~4000 chars |

`fetch` does not render JavaScript, so JS-only SPAs return little; typical
articles/wikis/docs work well. Search snippets alone often answer a question.

## Run

```sh
go run .                    # serve SSE on :8000  (endpoint /sse)
go run . -q "who won euro 2024"   # local test: search (-f week for a freshness window)
go run . -u "https://en.wikipedia.org/wiki/Go_(programming_language)"  # local test: fetch
```

Config: `ADDR` (default `:8000`), `REGION` (DuckDuckGo market, default `ua-uk`;
empty for none).

## Docker

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  -t <registry>/ddgo-mcp:0.1.0 --push .
```

Image is `distroless/static:nonroot` (~a few MB, ships CA certs, no shell).

## Behind kgateway / agentgateway

Deploy it as a `Deployment` + `Service` on `:8000` and add it as an MCP target
(`protocol: SSE`) on the gateway's MCP `Backend`. Tools then appear to clients
as `<target>_search` / `<target>_fetch` at the gateway endpoint.

## Notes

- HTTP client forces HTTP/1.1 — some CDNs reset the HTTP/2 stream for
  non-browser clients.
- Bot-protected sites (heavy WAFs) may time out on `fetch`; that's expected, the
  caller can fall back to search snippets.
- DuckDuckGo rate-limits by address: after a burst of requests it serves a
  CAPTCHA page (HTTP 202) for a while. `search` reports that as an error
  ("search unavailable … CAPTCHA") rather than as no results, keeps 1.5 s
  between requests and caches identical queries for 10 minutes, so a model
  retrying does not dig the hole deeper. Every call is logged with its outcome.
