package messages

import (
	"context"

	"github.com/d-kuro/kirocc/internal/auth"
	"github.com/d-kuro/kirocc/internal/kiroclient"
	"github.com/d-kuro/kirocc/internal/reqconv"
)

// TokenGetter loads valid upstream credentials for a request.
type TokenGetter interface {
	GetToken(ctx context.Context) (*auth.Credentials, error)
}

// Service owns message execution and token counting flows.
type Service struct {
	auth             TokenGetter
	client           kiroclient.Client
	captureEnabled   bool
	maxBodySize      int64
	maxHistoryImages int
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

// WithMaxHistoryImages caps how many earlier-turn images are replayed on the
// current message. Zero disables replay; negative means unlimited.
func WithMaxHistoryImages(n int) Option {
	return func(s *Service) { s.maxHistoryImages = n }
}

// New constructs a message service.
func New(authMgr TokenGetter, client kiroclient.Client, opts ...Option) *Service {
	s := &Service{
		auth:             authMgr,
		client:           client,
		maxBodySize:      DefaultMaxBodySize,
		maxHistoryImages: reqconv.DefaultMaxHistoryImages,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}
