package messages

import (
	"context"
	"time"

	"github.com/d-kuro/kirocc/internal/auth"
	"github.com/d-kuro/kirocc/internal/kiroclient"
	"github.com/d-kuro/kirocc/internal/kiromcp"
	"github.com/d-kuro/kirocc/internal/reqconv"
	"github.com/d-kuro/kirocc/internal/webfetch"
)

// TokenGetter loads valid upstream credentials for a request.
type TokenGetter interface {
	GetToken(ctx context.Context) (*auth.Credentials, error)
}

// Service owns message execution and token counting flows.
type Service struct {
	auth              TokenGetter
	client            kiroclient.Client
	mcp               kiromcp.Client
	captureEnabled    bool
	keepAliveInterval time.Duration
	maxBodySize       int64
	historyImageTurns int

	// Web-search behavior. webFetcher enriches search results with fetched
	// page content (nil disables enrichment); webSearchVisible emits searches
	// to the client as server_tool_use / web_search_tool_result blocks.
	webFetcher       *webfetch.Fetcher
	webFetchCount    int
	webFetchBytes    int
	webSearchVisible bool
}

// Option configures a Service.
type Option func(*Service)

// WithCapture enables recording of full upstream request/response bodies on
// failure for debugging. Defaults to disabled; callers should enable it only
// when debug logging is on.
func WithCapture(enabled bool) Option {
	return func(s *Service) { s.captureEnabled = enabled }
}

// WithMaxBodySize caps the accepted client request body in bytes. Zero or
// negative means unlimited.
func WithMaxBodySize(n int64) Option {
	return func(s *Service) { s.maxBodySize = n }
}

// WithHistoryImageTurns sets how many earlier user turns still replay their
// images on the current message. Zero disables replay; negative means unlimited.
func WithHistoryImageTurns(n int) Option {
	return func(s *Service) { s.historyImageTurns = n }
}

// WithMCPClient enables Kiro-hosted web search by supplying the InvokeMCP
// client used to execute it. A nil client (the default) leaves the feature off:
// Anthropic's WebSearch declaration is still stripped so the request succeeds,
// but no replacement tool is offered to the model.
func WithMCPClient(c kiromcp.Client) Option {
	return func(s *Service) { s.mcp = c }
}

// WithWebFetch enables enriching search results with fetched page content:
// the top count result URLs are downloaded and up to perPageBytes of readable
// text is attached per result. A nil fetcher or non-positive count disables
// enrichment, leaving snippets only.
func WithWebFetch(f *webfetch.Fetcher, count, perPageBytes int) Option {
	return func(s *Service) {
		s.webFetcher = f
		s.webFetchCount = count
		s.webFetchBytes = perPageBytes
	}
}

// WithWebSearchVisible controls whether executed searches are emitted to the
// client as server_tool_use / web_search_tool_result blocks (Anthropic's
// native shape, giving transcript persistence) or stay hidden. Defaults to
// visible.
func WithWebSearchVisible(v bool) Option {
	return func(s *Service) { s.webSearchVisible = v }
}

// webSearchEnabled reports whether Kiro-hosted web search is available.
func (s *Service) webSearchEnabled() bool { return s.mcp != nil }

// WithKeepAliveInterval sets the idle interval for SSE keep-alive comments.
// A zero duration disables the heartbeat.
func WithKeepAliveInterval(interval time.Duration) Option {
	return func(s *Service) { s.keepAliveInterval = interval }
}

// New constructs a message service.
func New(authMgr TokenGetter, client kiroclient.Client, opts ...Option) *Service {
	s := &Service{
		auth:              authMgr,
		client:            client,
		maxBodySize:       DefaultMaxBodySize,
		historyImageTurns: reqconv.DefaultHistoryImageTurns,
		webSearchVisible:  true,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}
