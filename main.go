// ddgo-mcp is a tiny MCP server exposing web search + readable page fetch —
// pure Go, no headless browser, no Python. It serves SSE (default :8000) so it
// drops behind an MCP gateway (kgateway/agentgateway) as a backend; the gateway
// namespaces the tools as <target>_search and <target>_fetch.
//
//	ddgo-mcp                    # serve SSE on :8000 (endpoint /sse)
//	ddgo-mcp -q "golang 1.25"   # local test: run a search and print (-f week for the freshness window)
//	ddgo-mcp -u "https://..."   # local test: fetch a page and print
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	readability "github.com/go-shiori/go-readability"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"golang.org/x/net/html"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

const (
	ddgHTML   = "https://html.duckduckgo.com/html/"
	userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
	maxBody   = 4 << 20 // 4 MiB cap on fetched pages
	maxText   = 4000    // runes of readable text returned by fetch
)

// region is DuckDuckGo's kl parameter: results ranked for this market and
// language rather than for the US. REGION="" turns it off.
var region = envOr("REGION", "ua-uk")

// httpClient forces HTTP/1.1: some CDNs reset the HTTP/2 stream for non-browser
// clients (INTERNAL_ERROR), which broke fetches.
var httpClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
		ForceAttemptHTTP2: false,
		MaxIdleConns:      100,
		IdleConnTimeout:   90 * time.Second,
	},
}

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8000"), "listen address for the SSE server")
	q := flag.String("q", "", "local test: run a search for this query and print")
	f := flag.String("f", "", "local test: freshness window for -q (day, week, month, year)")
	u := flag.String("u", "", "local test: fetch this URL and print readable text")
	flag.Parse()

	ctx := context.Background()
	switch {
	case *q != "":
		results, err := newSearcher(httpClient, region).search(ctx, *q, *f, 5)
		if err != nil {
			log.Fatalf("search: %v", err)
		}
		for i, r := range results {
			fmt.Printf("%d. %s\n   %s\n   %s\n", i+1, r.Title, r.URL, r.Snippet)
		}
	case *u != "":
		text, err := fetchReadable(ctx, *u)
		if err != nil {
			log.Fatalf("fetch: %v", err)
		}
		fmt.Println(text)
	default:
		serve(*addr)
	}
}

func serve(addr string) {
	s := server.NewMCPServer("ddgo-mcp", version)
	ddg := newSearcher(httpClient, region)

	s.AddTool(
		mcp.NewTool("search",
			mcp.WithDescription("Search the web via DuckDuckGo for current or factual information. Returns the top results as title, url and snippet. Often the snippets alone answer the question. For news, prices, weather or anything that changes, set freshness so old pages are left out."),
			mcp.WithString("query", mcp.Required(), mcp.Description("The search query")),
			mcp.WithNumber("max_results", mcp.Description("Number of results to return (default 5, max 10)")),
			mcp.WithString("freshness", mcp.Description("Only pages from the last day, week, month or year; omit for any time"),
				mcp.Enum("day", "week", "month", "year")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleSearch(ctx, ddg, req)
		},
	)

	s.AddTool(
		mcp.NewTool("fetch",
			mcp.WithDescription("Fetch a web page by URL and return its main readable text (no JavaScript rendering). Use after search when a snippet is not enough."),
			mcp.WithString("url", mcp.Required(), mcp.Description("The absolute http(s) URL to fetch")),
		),
		handleFetch,
	)

	// Mount the SSE handlers on our own mux so we can add /healthz for
	// Kubernetes liveness/readiness probes (the gateway connects to /sse).
	sse := server.NewSSEServer(s)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	mux.Handle("/sse", sse.SSEHandler())
	mux.Handle("/message", sse.MessageHandler())

	log.Printf("ddgo-mcp %s: listening on %s (/sse, /message, /healthz)", version, addr)
	if err := (&http.Server{Addr: addr, Handler: mux}).ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func handleSearch(ctx context.Context, ddg *searcher, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	n := req.GetInt("max_results", 5)
	if n < 1 {
		n = 1
	}
	if n > 10 {
		n = 10
	}
	fresh := req.GetString("freshness", "")
	if fresh != "" && freshness[fresh] == "" {
		return mcp.NewToolResultError("freshness must be day, week, month or year"), nil
	}

	start := time.Now()
	results, err := ddg.search(ctx, query, fresh, n)
	logSearch(query, fresh, len(results), err, time.Since(start))
	if errors.Is(err, ErrChallenge) {
		// Said plainly, so the model tells the person rather than answering
		// from memory as if the web had nothing on it.
		return mcp.NewToolResultError("search unavailable: DuckDuckGo is rate-limiting this address right now (CAPTCHA). Do not retry at once; tell the user the web search is temporarily unavailable, or try again in a few minutes."), nil
	}
	if err != nil {
		return mcp.NewToolResultError("search failed: " + err.Error()), nil
	}
	if len(results) == 0 {
		return mcp.NewToolResultText("No results found."), nil
	}

	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n%s\n%s\n\n", i+1, r.Title, r.URL, r.Snippet)
	}
	return mcp.NewToolResultText(strings.TrimSpace(b.String())), nil
}

func handleFetch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	pageURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	start := time.Now()
	text, err := fetchReadable(ctx, pageURL)
	if err != nil {
		log.Printf("fetch %s: %v (%s)", pageURL, err, time.Since(start).Round(time.Millisecond))
		return mcp.NewToolResultError("fetch failed: " + err.Error()), nil
	}
	log.Printf("fetch %s: %d chars (%s)", pageURL, len(text), time.Since(start).Round(time.Millisecond))
	return mcp.NewToolResultText(text), nil
}

// ---- fetch (readability) -------------------------------------------------

var wsRun = regexp.MustCompile(`[ \t]*\n[ \t\n]*\n[ \t]*`)

func fetchReadable(ctx context.Context, pageURL string) (string, error) {
	u, err := url.Parse(pageURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("invalid url %q", pageURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}

	art, err := readability.FromReader(io.LimitReader(resp.Body, maxBody), u)
	if err != nil {
		return "", err
	}

	text := strings.TrimSpace(wsRun.ReplaceAllString(art.TextContent, "\n\n"))
	if r := []rune(text); len(r) > maxText {
		text = string(r[:maxText]) + "…"
	}
	var head []string
	if title := strings.TrimSpace(art.Title); title != "" {
		head = append(head, title)
	}
	// The page's own dates, when it declares them: the one thing the model
	// needs to tell a current article from a stale one.
	if art.PublishedTime != nil {
		head = append(head, "Published: "+art.PublishedTime.Format("2006-01-02"))
	}
	if art.ModifiedTime != nil && (art.PublishedTime == nil || !art.ModifiedTime.Equal(*art.PublishedTime)) {
		head = append(head, "Updated: "+art.ModifiedTime.Format("2006-01-02"))
	}
	if len(head) > 0 {
		return strings.Join(head, "\n") + "\n\n" + text, nil
	}
	return text, nil
}

// ---- html helpers --------------------------------------------------------

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, cls string) bool {
	for _, c := range strings.Fields(attr(n, "class")) {
		if c == cls {
			return true
		}
	}
	return false
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
