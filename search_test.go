package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A DuckDuckGo HTML page as the endpoint shapes it: an ad first, then two
// organic results, the second one without a snippet.
const ddgPage = `<html><body><div id="links" class="results">
<div class="result results_links results_links_deep result--ad">
 <div class="links_main links_deep result__body">
  <h2 class="result__title"><a class="result__a" href="https://duckduckgo.com/y.js?ad=1">Buy now</a></h2>
  <a class="result__snippet" href="https://duckduckgo.com/y.js?ad=1">An advert.</a>
 </div></div>
<div class="result results_links results_links_deep web-result">
 <div class="links_main links_deep result__body">
  <h2 class="result__title"><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fa&amp;rut=1">First <b>result</b></a></h2>
  <a class="result__snippet" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fa">The first snippet.</a>
 </div></div>
<div class="result results_links results_links_deep web-result">
 <div class="links_main links_deep result__body">
  <h2 class="result__title"><a class="result__a" href="https://example.com/b">Second result</a></h2>
 </div></div>
</div></body></html>`

const challengePage = `<html><body><div class="anomaly-modal__title">Unfortunately, bots use DuckDuckGo too.</div>
<p>Please complete the following challenge to confirm this search was made by a human.</p></body></html>`

func TestParseResults(t *testing.T) {
	got, err := parseResults([]byte(ddgPage))
	if err != nil {
		t.Fatal(err)
	}
	want := []result{
		{Title: "First result", URL: "https://example.com/a", Snippet: "The first snippet."},
		{Title: "Second result", URL: "https://example.com/b"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d results %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("result %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSearch(t *testing.T) {
	var calls int
	var lastQuery string
	var page string
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		lastQuery = r.URL.RawQuery
		w.WriteHeader(status)
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()
	s := newSearcher(srv.Client(), "ua-uk")
	// The test server stands in for the endpoint: the searcher only ever
	// appends the query to ddgHTML, so point ddgHTML's host at it.
	s.client.Transport = rewriteHost(srv.URL)

	page, status = ddgPage, http.StatusOK
	got, err := s.search(context.Background(), "test query", "week", 1)
	if err != nil || len(got) != 1 || got[0].URL != "https://example.com/a" {
		t.Fatalf("search = %+v, %v", got, err)
	}
	for _, want := range []string{"q=test+query", "kl=ua-uk", "df=w"} {
		if !contains(lastQuery, want) {
			t.Errorf("query %q lacks %s", lastQuery, want)
		}
	}
	// The same query again comes from the cache: no second request.
	if _, err := s.search(context.Background(), "test query", "week", 5); err != nil || calls != 1 {
		t.Errorf("cached search: %v, %d calls", err, calls)
	}
	// Another window is another request, and the gap is kept.
	start := time.Now()
	if _, err := s.search(context.Background(), "test query", "", 5); err != nil || calls != 2 {
		t.Errorf("new window: %v, %d calls", err, calls)
	}
	if time.Since(start) < minGap-100*time.Millisecond {
		t.Errorf("second request after %s, want the %s gap", time.Since(start), minGap)
	}

	// The CAPTCHA page, as a 202 and as a 200, is the challenge error, not
	// an empty result.
	for _, st := range []int{http.StatusAccepted, http.StatusOK} {
		page, status = challengePage, st
		_, err := s.search(context.Background(), "blocked "+time.Now().String(), "", 5)
		if !errors.Is(err, ErrChallenge) {
			t.Errorf("http %d challenge page: err = %v, want ErrChallenge", st, err)
		}
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// rewriteHost sends every request to the test server, whatever host the
// request names.
func rewriteHost(target string) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r2 := r.Clone(r.Context())
		r2.URL.Scheme = "http"
		r2.URL.Host = target[len("http://"):]
		return http.DefaultTransport.RoundTrip(r2)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
