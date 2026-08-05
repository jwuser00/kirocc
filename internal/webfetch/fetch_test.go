package webfetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testFetcher returns a Fetcher whose client can reach httptest loopback servers.
func testFetcher(opts ...Option) *Fetcher {
	return New(append([]Option{WithHTTPClient(&http.Client{Timeout: 5 * time.Second})}, opts...)...)
}

const samplePage = `<!doctype html>
<html><head><title>Go 1.26 Release Notes</title><style>body{color:red}</style></head>
<body>
<nav><a href="/">Home</a><a href="/docs">Docs</a></nav>
<header>Site header boilerplate</header>
<article>
<h1>Go 1.26</h1>
<p>The latest Go release introduces several improvements to the runtime,
including faster garbage collection and improved scheduling latency for
highly concurrent workloads across all supported platforms today.</p>
<p>Generics diagnostics are clearer, and the toolchain now defaults to
module graph pruning in workspaces, reducing dependency resolution time
noticeably in large monorepos with many nested modules everywhere.</p>
<script>console.log("tracker")</script>
</article>
<footer>Copyright footer</footer>
</body></html>`

func TestFetch_ExtractsArticleDropsChrome(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(samplePage))
	}))
	defer srv.Close()

	pages := testFetcher().FetchAll(context.Background(), []string{srv.URL}, 0)
	if len(pages) != 1 {
		t.Fatalf("pages = %d, want 1", len(pages))
	}
	p := pages[0]
	if p.Err != nil {
		t.Fatalf("fetch: %v", p.Err)
	}
	if p.Title != "Go 1.26 Release Notes" {
		t.Errorf("title = %q", p.Title)
	}
	for _, want := range []string{"Go 1.26", "faster garbage collection", "module graph pruning"} {
		if !strings.Contains(p.Content, want) {
			t.Errorf("content missing %q:\n%s", want, p.Content)
		}
	}
	for _, unwanted := range []string{"Site header boilerplate", "Copyright footer", "Docs", "tracker", "color:red"} {
		if strings.Contains(p.Content, unwanted) {
			t.Errorf("content contains chrome %q:\n%s", unwanted, p.Content)
		}
	}
}

func TestFetch_TruncatesAtByteBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><p>" + strings.Repeat("word ", 500) + "</p></body></html>"))
	}))
	defer srv.Close()

	p := testFetcher().FetchAll(context.Background(), []string{srv.URL}, 100)[0]
	if p.Err != nil {
		t.Fatalf("fetch: %v", p.Err)
	}
	if len(p.Content) > 100+len("\n[content truncated]") {
		t.Errorf("content length = %d, want <= budget+marker", len(p.Content))
	}
	if !strings.HasSuffix(p.Content, "[content truncated]") {
		t.Errorf("truncated content missing marker: %q", p.Content)
	}
}

func TestFetch_CachesByURL(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><p>cached page body</p></body></html>"))
	}))
	defer srv.Close()

	f := testFetcher()
	for range 3 {
		p := f.FetchAll(context.Background(), []string{srv.URL}, 0)[0]
		if p.Err != nil {
			t.Fatalf("fetch: %v", p.Err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("origin fetched %d times, want 1 (cache)", n)
	}
}

func TestFetch_ErrorsAreNotCached(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><p>recovered</p></body></html>"))
	}))
	defer srv.Close()

	f := testFetcher()
	if p := f.FetchAll(context.Background(), []string{srv.URL}, 0)[0]; p.Err == nil {
		t.Fatal("first fetch should fail with 500")
	}
	if p := f.FetchAll(context.Background(), []string{srv.URL}, 0)[0]; p.Err != nil {
		t.Fatalf("second fetch should succeed: %v", p.Err)
	}
}

func TestFetch_RejectsNonTextContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.7"))
	}))
	defer srv.Close()

	p := testFetcher().FetchAll(context.Background(), []string{srv.URL}, 0)[0]
	if p.Err == nil || !strings.Contains(p.Err.Error(), "unsupported content type") {
		t.Errorf("err = %v, want unsupported content type", p.Err)
	}
}

func TestFetch_PlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("plain text document\n"))
	}))
	defer srv.Close()

	p := testFetcher().FetchAll(context.Background(), []string{srv.URL}, 0)[0]
	if p.Err != nil {
		t.Fatalf("fetch: %v", p.Err)
	}
	if p.Content != "plain text document" {
		t.Errorf("content = %q", p.Content)
	}
}

func TestFetchAll_PreservesOrderAndIsolatesFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "bad") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><p>page " + r.URL.Path + "</p></body></html>"))
	}))
	defer srv.Close()

	urls := []string{srv.URL + "/a", srv.URL + "/bad", srv.URL + "/c", "::not a url::"}
	pages := testFetcher().FetchAll(context.Background(), urls, 0)
	if len(pages) != 4 {
		t.Fatalf("pages = %d", len(pages))
	}
	if pages[0].Err != nil || !strings.Contains(pages[0].Content, "page /a") {
		t.Errorf("pages[0] = %+v", pages[0])
	}
	if pages[1].Err == nil {
		t.Error("pages[1] should fail with 404")
	}
	if pages[2].Err != nil || !strings.Contains(pages[2].Content, "page /c") {
		t.Errorf("pages[2] = %+v", pages[2])
	}
	if pages[3].Err == nil {
		t.Error("pages[3] should fail on unparseable url")
	}
	for i, u := range urls {
		if pages[i].URL != u {
			t.Errorf("pages[%d].URL = %q, want %q (order preserved)", i, pages[i].URL, u)
		}
	}
}

func TestBlockPrivateAddr(t *testing.T) {
	blocked := []string{
		"127.0.0.1:80", "10.0.0.1:443", "192.168.1.1:8080", "172.16.0.1:80",
		"169.254.169.254:80", // cloud metadata endpoint
		"[::1]:443", "0.0.0.0:80", "[fe80::1]:80",
	}
	for _, addr := range blocked {
		if err := blockPrivateAddr("tcp", addr, nil); err == nil {
			t.Errorf("blockPrivateAddr(%q) = nil, want blocked", addr)
		}
	}
	allowed := []string{"93.184.216.34:443", "[2606:2800:220:1:248:1893:25c8:1946]:443"}
	for _, addr := range allowed {
		if err := blockPrivateAddr("tcp", addr, nil); err != nil {
			t.Errorf("blockPrivateAddr(%q) = %v, want allowed", addr, err)
		}
	}
}

func TestDefaultFetcher_BlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>secret</body></html>"))
	}))
	defer srv.Close()

	// The real (non-test) client must refuse to dial the loopback server.
	p := New().FetchAll(context.Background(), []string{srv.URL}, 0)[0]
	if p.Err == nil {
		t.Fatal("default fetcher fetched a loopback URL; SSRF guard missing")
	}
}

func TestExtractText_FallsBackToBodyWhenArticleTooShort(t *testing.T) {
	page := `<html><head><title>t</title></head><body>
	<article>teaser</article>
	<div><p>` + strings.Repeat("real body content ", 30) + `</p></div>
	</body></html>`
	_, text, err := ExtractText(strings.NewReader(page), "text/html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "real body content") {
		t.Errorf("text = %q, want body fallback", text)
	}
}

func TestTruncate_RuneBoundary(t *testing.T) {
	s := strings.Repeat("한", 100) // 3 bytes each
	out := Truncate(s, 10)
	cut, _, _ := strings.Cut(out, "\n")
	if len(cut) != 9 { // 3 whole runes
		t.Errorf("cut length = %d, want 9 (rune boundary)", len(cut))
	}
	for _, r := range cut {
		if r != '한' {
			t.Errorf("corrupt rune %q in %q", r, cut)
		}
	}
}
