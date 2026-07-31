// Package kiromcp calls the MCP server AWS hosts behind the Kiro subscription.
//
// kiro-cli implements its built-in tools (currently only web_search) by issuing
// a JSON-RPC 2.0 request to the AmazonCodeWhispererStreamingService.InvokeMCP
// operation on q.<region>.amazonaws.com. That endpoint accepts the very same
// bearer token kirocc already reads from the Kiro CLI database for
// GenerateAssistantResponse, so no separate credential or third-party search
// API key is involved. Usage is metered against the Kiro subscription.
//
// The wire shape below was captured from kiro-cli 2.15.1; it is not a
// documented AWS contract and may change without notice. Callers are expected
// to degrade gracefully (surface the failure to the model as a tool error)
// rather than fail the whole conversation.
package kiromcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	amzTarget = "AmazonCodeWhispererStreamingService.InvokeMCP"

	// maxAttempts is the total number of request attempts (initial + retries).
	maxAttempts    = 3
	baseRetryDelay = 500 * time.Millisecond

	defaultTimeout = 60 * time.Second
	bodyLimit      = 1 << 20 // 1 MiB; a 10-result web_search reply is ~5 KB
)

// ErrNoContent is returned when the MCP server answers without a usable
// content block. Treated as a tool-level failure, not a transport failure.
var ErrNoContent = errors.New("kiromcp: response contained no text content")

// Result is the outcome of a tools/call.
//
// IsError mirrors the MCP `isError` flag: the call completed but the tool
// itself failed, and Text holds the tool's own error message. That maps
// directly onto an Anthropic tool_result block with is_error set.
type Result struct {
	Text    string
	IsError bool
}

// TokenRefresher is called when a 403 is received to obtain a fresh token.
type TokenRefresher func(ctx context.Context) (newToken string, err error)

// Client calls tools on the Kiro-hosted MCP server.
type Client interface {
	CallTool(ctx context.Context, token, profileARN, region, name string, args map[string]any) (*Result, error)
	ListTools(ctx context.Context, token, profileARN, region string) ([]Tool, error)
}

// Tool is one entry from a tools/list response.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// HTTPClient is the production implementation of Client.
type HTTPClient struct {
	httpClient     *http.Client
	baseURL        string // full endpoint override; empty = region-based URL
	tokenRefresher TokenRefresher
}

// HTTPClientOption configures an HTTPClient.
type HTTPClientOption func(*HTTPClient)

// WithBaseURL sets a custom endpoint, bypassing region-based URL construction.
func WithBaseURL(url string) HTTPClientOption {
	return func(c *HTTPClient) { c.baseURL = url }
}

// WithTokenRefresher sets the token refresh callback used on 403.
func WithTokenRefresher(fn TokenRefresher) HTTPClientOption {
	return func(c *HTTPClient) { c.tokenRefresher = fn }
}

// WithHTTPClient overrides the underlying HTTP client.
func WithHTTPClient(hc *http.Client) HTTPClientOption {
	return func(c *HTTPClient) { c.httpClient = hc }
}

// NewHTTPClient creates a new HTTPClient.
func NewHTTPClient(opts ...HTTPClientOption) *HTTPClient {
	c := &HTTPClient{httpClient: &http.Client{Timeout: defaultTimeout}}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *HTTPClient) endpointURL(region string) string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return fmt.Sprintf("https://q.%s.amazonaws.com/", region)
}

// rpcRequest is the JSON-RPC 2.0 envelope InvokeMCP expects. profileArn sits
// alongside the JSON-RPC fields rather than inside params — that is how
// kiro-cli sends it.
type rpcRequest struct {
	ProfileARN string `json:"profileArn,omitempty"`
	JSONRPC    string `json:"jsonrpc"`
	ID         string `json:"id"`
	Method     string `json:"method"`
	Params     any    `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type rpcResponse struct {
	Error  *rpcError `json:"error,omitzero"`
	Result struct {
		Content []contentBlock `json:"content"`
		IsError bool           `json:"isError"`
		Tools   []Tool         `json:"tools"`
	} `json:"result"`
}

// CallTool invokes a tool via tools/call and returns its text content.
func (c *HTTPClient) CallTool(ctx context.Context, token, profileARN, region, name string, args map[string]any) (*Result, error) {
	resp, err := c.do(ctx, token, profileARN, region, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return nil, err
	}

	var sb strings.Builder
	for _, b := range resp.Result.Content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	if sb.Len() == 0 {
		return nil, ErrNoContent
	}
	return &Result{Text: sb.String(), IsError: resp.Result.IsError}, nil
}

// ListTools enumerates the tools the MCP server exposes.
func (c *HTTPClient) ListTools(ctx context.Context, token, profileARN, region string) ([]Tool, error) {
	resp, err := c.do(ctx, token, profileARN, region, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	return resp.Result.Tools, nil
}

// do performs the JSON-RPC call with retry on 403 (after refresh), 429 and 5xx.
func (c *HTTPClient) do(ctx context.Context, token, profileARN, region, method string, params any) (*rpcResponse, error) {
	endpoint := c.endpointURL(region)
	body, err := json.Marshal(&rpcRequest{
		ProfileARN: profileARN,
		JSONRPC:    "2.0",
		ID:         uuid.New().String(),
		Method:     method,
		Params:     params,
	})
	if err != nil {
		return nil, fmt.Errorf("kiromcp: marshal request: %w", err)
	}

	currentToken := token
	var lastErr error

	for attempt := range maxAttempts {
		if attempt > 0 {
			if werr := wait(ctx, backoffDelay(attempt-1)); werr != nil {
				return nil, werr
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("kiromcp: create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-amz-json-1.0")
		req.Header.Set("X-Amz-Target", amzTarget)
		req.Header.Set("Authorization", "Bearer "+currentToken)
		req.Header.Set("X-Amzn-Codewhisperer-Optout", "false")
		if profileARN != "" {
			req.Header.Set("X-Amzn-Kiro-Profile-Arn", profileARN)
		}

		httpResp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("kiromcp: %s: %w", method, err)
			continue
		}

		switch {
		case httpResp.StatusCode == http.StatusOK:
			raw, err := io.ReadAll(io.LimitReader(httpResp.Body, bodyLimit))
			_ = httpResp.Body.Close()
			if err != nil {
				lastErr = fmt.Errorf("kiromcp: read body: %w", err)
				continue
			}
			var parsed rpcResponse
			if err := json.Unmarshal(raw, &parsed); err != nil {
				return nil, fmt.Errorf("kiromcp: decode response: %w", err)
			}
			if parsed.Error != nil {
				return nil, fmt.Errorf("kiromcp: %s: rpc error %d: %s", method, parsed.Error.Code, parsed.Error.Message)
			}
			return &parsed, nil

		case httpResp.StatusCode == http.StatusForbidden && c.tokenRefresher != nil:
			snippet := readLimited(httpResp.Body)
			lastErr = fmt.Errorf("kiromcp: %s: 403: %s", method, snippet)
			newToken, rerr := c.tokenRefresher(ctx)
			if rerr != nil {
				return nil, fmt.Errorf("kiromcp: token refresh: %w", rerr)
			}
			currentToken = newToken
			slog.DebugContext(ctx, "kiromcp: refreshed token after 403", "method", method)

		case httpResp.StatusCode == http.StatusTooManyRequests || httpResp.StatusCode >= 500:
			snippet := readLimited(httpResp.Body)
			lastErr = fmt.Errorf("kiromcp: %s: status %d: %s", method, httpResp.StatusCode, snippet)

		default:
			snippet := readLimited(httpResp.Body)
			return nil, fmt.Errorf("kiromcp: %s: status %d: %s", method, httpResp.StatusCode, snippet)
		}
	}

	return nil, lastErr
}

func readLimited(body io.ReadCloser) string {
	b, _ := io.ReadAll(io.LimitReader(body, 4096))
	_ = body.Close()
	return string(b)
}

// backoffDelay returns exponential backoff with ±25% jitter.
func backoffDelay(attempt int) time.Duration {
	base := baseRetryDelay << attempt
	return base + time.Duration(rand.Int64N(int64(base)/2)) - base/4
}

func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
