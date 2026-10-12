package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

// ErrChallenge is DuckDuckGo refusing to answer: it serves a CAPTCHA page
// (HTTP 202, "Unfortunately, bots use DuckDuckGo too") once an address has
// asked too often. It used to be parsed as a page with no results, so the
// model heard "No results found." and answered from memory, which is how a
// stale answer looks to the person asking.
var ErrChallenge = errors.New("duckduckgo is asking for a CAPTCHA from this address (rate limit)")

// freshness maps the tool's freshness argument to DuckDuckGo's df parameter.
var freshness = map[string]string{"day": "d", "week": "w", "month": "m", "year": "y"}

type result struct {
	Title   string
	URL     string
	Snippet string
}

// searcher queries DuckDuckGo's HTML endpoint, no more than one request per
// minGap and never the same query twice within cacheTTL. Both exist for the
// rate limit: a model retrying a failed search three times in a row is exactly
// the burst that earns the CAPTCHA, and the CAPTCHA then lasts for everyone.
type searcher struct {
	client *http.Client
	region string

	mu    sync.Mutex
	last  time.Time
	cache map[string]cacheEntry
}

type cacheEntry struct {
	at      time.Time
	results []result
}

const (
	minGap     = 1500 * time.Millisecond
	cacheTTL   = 10 * time.Minute
	cacheLimit = 128
)

func newSearcher(client *http.Client, region string) *searcher {
	return &searcher{client: client, region: region, cache: map[string]cacheEntry{}}
}

// search returns up to n results for query, within the freshness window
// ("" for any time). The cache is keyed on both.
func (s *searcher) search(ctx context.Context, query, fresh string, n int) ([]result, error) {
	key := fresh + "\x00" + query
	s.mu.Lock()
	if e, ok := s.cache[key]; ok && time.Since(e.at) < cacheTTL {
		s.mu.Unlock()
		return clip(e.results, n), nil
	}
	if wait := minGap - time.Since(s.last); wait > 0 {
		s.mu.Unlock()
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		s.mu.Lock()
	}
	s.last = time.Now()
	s.mu.Unlock()

	results, err := s.fetchResults(ctx, query, fresh)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if len(s.cache) >= cacheLimit {
		for k, e := range s.cache {
			if time.Since(e.at) >= cacheTTL {
				delete(s.cache, k)
			}
		}
		if len(s.cache) >= cacheLimit {
			s.cache = map[string]cacheEntry{}
		}
	}
	s.cache[key] = cacheEntry{at: time.Now(), results: results}
	s.mu.Unlock()
	return clip(results, n), nil
}

func clip(r []result, n int) []result {
	if len(r) > n {
		return r[:n]
	}
	return r
}

func (s *searcher) fetchResults(ctx context.Context, query, fresh string) ([]result, error) {
	q := url.Values{"q": {query}}
	if s.region != "" {
		q.Set("kl", s.region)
	}
	if df := freshness[fresh]; df != "" {
		q.Set("df", df)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ddgHTML+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "uk,en;q=0.8")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusTooManyRequests || isChallenge(body) {
		return nil, ErrChallenge
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("duckduckgo returned http %d", resp.StatusCode)
	}
	return parseResults(body)
}

// isChallenge recognises the CAPTCHA page by its wording, for the day it
// comes with a 200.
func isChallenge(body []byte) bool {
	return strings.Contains(string(body), "bots use DuckDuckGo") || strings.Contains(string(body), "anomaly-modal")
}

// parseResults reads the organic results of a DuckDuckGo HTML page. Each
// result is a div.result holding its own a.result__a and .result__snippet, and
// they are paired inside that block: pairing the page's links and snippets by
// index, as before, shifted every description by one past a result without a
// snippet. Ads (div.result--ad) are skipped.
func parseResults(body []byte) ([]result, error) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	var out []result
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && hasClass(node, "result") {
			if !hasClass(node, "result--ad") {
				if r, ok := parseResult(node); ok {
					out = append(out, r)
				}
			}
			return // a result holds no other result
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out, nil
}

func parseResult(block *html.Node) (result, bool) {
	var r result
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch {
			case node.Data == "a" && hasClass(node, "result__a") && r.URL == "":
				r.Title = textOf(node)
				r.URL = decodeDDGURL(attr(node, "href"))
			case hasClass(node, "result__snippet") && r.Snippet == "":
				r.Snippet = textOf(node)
			}
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(block)
	return r, r.URL != ""
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

// logSearch is the one line per call that says what happened: the CAPTCHA
// went unnoticed for want of it.
func logSearch(query, fresh string, n int, err error, took time.Duration) {
	if err != nil {
		log.Printf("search %q fresh=%q: %v (%s)", query, fresh, err, took.Round(time.Millisecond))
		return
	}
	log.Printf("search %q fresh=%q: %d results (%s)", query, fresh, n, took.Round(time.Millisecond))
}
