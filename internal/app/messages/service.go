package messages

import (
	"context"

	"github.com/d-kuro/kirocc/internal/auth"
	"github.com/d-kuro/kirocc/internal/kiroclient"
	"github.com/d-kuro/kirocc/internal/kiromcp"
	"github.com/d-kuro/kirocc/internal/reqconv"
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
	maxBodySize       int64
	historyImageTurns int
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

// webSearchEnabled reports whether Kiro-hosted web search is available.
func (s *Service) webSearchEnabled() bool { return s.mcp != nil }

// New constructs a message service.
func New(authMgr TokenGetter, client kiroclient.Client, opts ...Option) *Service {
	s := &Service{
		auth:              authMgr,
		client:            client,
		maxBodySize:       DefaultMaxBodySize,
		historyImageTurns: reqconv.DefaultHistoryImageTurns,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}
