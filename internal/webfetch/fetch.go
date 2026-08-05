// Package webfetch downloads web pages referenced by search results and
// extracts their readable text, so the model receives page content rather than
// only titles and snippets — the depth Anthropic's hosted web search provides
// natively and the Kiro-hosted search does not.
package webfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultTimeout     = 8 * time.Second
	defaultConcurrency = 4
	defaultCacheSize   = 128
	defaultCacheTTL    = 15 * time.Minute
	maxRedirects       = 5
	// maxDownloadBytes caps how much of a page body is read; readable text
	// worth keeping fits well within this, and it bounds memory per fetch.
	maxDownloadBytes = 2 << 20

	// userAgent identifies the fetcher honestly while staying in the
	// Mozilla-compatible form many origins require to serve full pages.
	userAgent = "Mozilla/5.0 (compatible; kirocc/1.0; +https://github.com/d-kuro/kirocc)"
)

// errPrivateAddress marks a connection attempt into private/loopback space.
// Search results carry attacker-influenced URLs; kirocc runs next to local
// services (including itself), so those never get dialed.
var errPrivateAddress = errors.New("webfetch: connection to private address blocked")

// Page is one fetched-and-extracted page. Err is set instead of Content when
// the page could not be fetched or parsed; both are never set together.
type Page struct {
	URL     string
	Title   string
	Content string
	Err     error
}

// Fetcher downloads pages concurrently with a shared cache.
type Fetcher struct {
	client      *http.Client
	cache       *ttlCache
	concurrency int
}

// Option configures a Fetcher.
type Option func(*Fetcher)

// WithHTTPClient replaces the SSRF-guarded default client. Intended for tests,
// which need to reach httptest servers on loopback.
func WithHTTPClient(c *http.Client) Option {
	return func(f *Fetcher) { f.client = c }
}

// WithConcurrency sets how many pages are fetched in parallel.
func WithConcurrency(n int) Option {
	return func(f *Fetcher) {
		if n > 0 {
			f.concurrency = n
		}
	}
}

// New constructs a Fetcher with an SSRF-guarded HTTP client.
func New(opts ...Option) *Fetcher {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: blockPrivateAddr,
	}
	f := &Fetcher{
		client: &http.Client{
			Timeout: defaultTimeout,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				DialContext:         dialer.DialContext,
				TLSHandshakeTimeout: 5 * time.Second,
				MaxIdleConns:        16,
				IdleConnTimeout:     30 * time.Second,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return fmt.Errorf("webfetch: more than %d redirects", maxRedirects)
				}
				return nil
			},
		},
		cache:       newTTLCache(defaultCacheSize, defaultCacheTTL),
		concurrency: defaultConcurrency,
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// blockPrivateAddr rejects dials to non-public addresses. Runs after DNS
// resolution, so a hostname resolving to 127.0.0.1 is caught too.
func blockPrivateAddr(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("webfetch: unparseable dial address %q: %w", address, err)
	}
	addr := addrPort.Addr().Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return errPrivateAddress
	}
	return nil
}

// FetchAll fetches urls concurrently, preserving input order in the result.
// Individual failures land in Page.Err; FetchAll itself never fails.
// Extracted content is clamped to maxBytes per page.
func (f *Fetcher) FetchAll(ctx context.Context, urls []string, maxBytes int) []Page {
	pages := make([]Page, len(urls))
	sem := make(chan struct{}, f.concurrency)
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			pages[i] = f.fetch(ctx, u, maxBytes)
		})
	}
	wg.Wait()
	return pages
}

// fetch returns one page, from cache when possible. The cache stores the full
// extracted text; truncation to maxBytes happens per call so different budgets
// share one entry.
func (f *Fetcher) fetch(ctx context.Context, pageURL string, maxBytes int) Page {
	if cached, ok := f.cache.get(pageURL); ok {
		cached.Content = Truncate(cached.Content, maxBytes)
		return cached
	}
	page := f.fetchRemote(ctx, pageURL)
	if page.Err == nil {
		f.cache.put(pageURL, page)
	}
	page.Content = Truncate(page.Content, maxBytes)
	return page
}

func (f *Fetcher) fetchRemote(ctx context.Context, pageURL string) Page {
	page := Page{URL: pageURL}

	u, err := url.Parse(pageURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		page.Err = fmt.Errorf("webfetch: unsupported url %q", pageURL)
		return page
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		page.Err = fmt.Errorf("webfetch: build request: %w", err)
		return page
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.1")

	resp, err := f.client.Do(req)
	if err != nil {
		page.Err = fmt.Errorf("webfetch: get: %w", err)
		return page
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		page.Err = fmt.Errorf("webfetch: %s returned status %d", pageURL, resp.StatusCode)
		return page
	}

	contentType := resp.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	body := io.LimitReader(resp.Body, maxDownloadBytes)

	switch mediaType {
	case "text/plain":
		raw, err := io.ReadAll(body)
		if err != nil {
			page.Err = fmt.Errorf("webfetch: read body: %w", err)
			return page
		}
		page.Content = strings.TrimSpace(string(raw))
	case "", "text/html", "application/xhtml+xml":
		title, text, err := ExtractText(body, contentType)
		if err != nil {
			page.Err = fmt.Errorf("webfetch: parse html: %w", err)
			return page
		}
		page.Title = title
		page.Content = text
	default:
		page.Err = fmt.Errorf("webfetch: unsupported content type %q", mediaType)
	}
	return page
}
