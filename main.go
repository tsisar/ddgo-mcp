// ddgo-mcp is a tiny MCP server exposing web search + readable page fetch —
// pure Go, no headless browser, no Python. It serves SSE (default :8000) so it
// drops behind an MCP gateway (kgateway/agentgateway) as a backend; the gateway
// namespaces the tools as <target>_search and <target>_fetch.
//
//	ddgo-mcp                    # serve SSE on :8000 (endpoint /sse)
//	ddgo-mcp -q "golang 1.25"   # local test: run a search and print
//	ddgo-mcp -u "https://..."   # local test: fetch a page and print
package main

import (
	"context"
	"crypto/tls"
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
	u := flag.String("u", "", "local test: fetch this URL and print readable text")
	flag.Parse()

	ctx := context.Background()
	switch {
	case *q != "":
		results, err := ddgSearch(ctx, *q, 5)
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

	s.AddTool(
		mcp.NewTool("search",
			mcp.WithDescription("Search the web via DuckDuckGo for current or factual information. Returns the top results as title, url and snippet. Often the snippets alone answer the question."),
			mcp.WithString("query", mcp.Required(), mcp.Description("The search query")),
			mcp.WithNumber("max_results", mcp.Description("Number of results to return (default 5, max 10)")),
		),
		handleSearch,
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

func handleSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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

	results, err := ddgSearch(ctx, query, n)
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
	text, err := fetchReadable(ctx, pageURL)
	if err != nil {
		return mcp.NewToolResultError("fetch failed: " + err.Error()), nil
	}
	return mcp.NewToolResultText(text), nil
}

// ---- search --------------------------------------------------------------

type result struct {
	Title   string
	URL     string
	Snippet string
}

func ddgSearch(ctx context.Context, query string, n int) ([]result, error) {
	endpoint := ddgHTML + "?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("duckduckgo returned http %d", resp.StatusCode)
	}

	doc, err := html.Parse(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}

	var links []result
	var snippets []string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch {
			case node.Data == "a" && hasClass(node, "result__a"):
				links = append(links, result{Title: textOf(node), URL: decodeDDGURL(attr(node, "href"))})
			case hasClass(node, "result__snippet"):
				snippets = append(snippets, textOf(node))
			}
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	out := make([]result, 0, n)
	for i := range links {
		if i >= n {
			break
		}
		r := links[i]
		if i < len(snippets) {
			r.Snippet = snippets[i]
		}
		out = append(out, r)
	}
	return out, nil
}

// decodeDDGURL turns a DuckDuckGo redirect href (//duckduckgo.com/l/?uddg=...)
// into the real destination URL.
func decodeDDGURL(href string) string {
	if href == "" {
		return ""
	}
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if real := u.Query().Get("uddg"); real != "" {
		return real
	}
	return href
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
	if title := strings.TrimSpace(art.Title); title != "" {
		return title + "\n\n" + text, nil
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
