package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"time"

	"github.com/d-kuro/kirocc/internal/logging"
)

const (
	// DefaultOTelBodyLimit is the default max bytes of request body to capture in OTel spans.
	DefaultOTelBodyLimit = 32 * 1024
	// DefaultKeepAliveInterval is the default idle time between SSE keep-alive comments.
	DefaultKeepAliveInterval = 15 * time.Second
)

// Config is the runtime configuration for kirocc.
type Config struct {
	Port   int
	Host   string
	DBPath string
	APIKey string // guards kirocc's own endpoints; unrelated to KiroAPIKey
	// KiroAPIKey is a Kiro API key ("ksk_…") used upstream instead of the
	// kiro-cli database credential. Named after Kiro's own KIRO_API_KEY rather
	// than the KIROCC_* convention, since it is Kiro's credential, not kirocc's.
	KiroAPIKey string
	// Region pins the region used by Kiro runtime and model catalog endpoints,
	// and is the region targeted in API-key mode (an API key carries no region
	// of its own). Empty means "use the region resolved from the credential
	// store", or auth's default in API-key mode.
	Region string
	// BaseURL overrides the whole Kiro runtime endpoint, bypassing region-based
	// URL construction. Empty means "derive from region". Intended for proxies
	// and debugging.
	BaseURL string
	// MaxBodySize caps an incoming client request body in bytes.
	// Zero or negative means unlimited.
	MaxBodySize int64
	// HistoryImageTurns is how many earlier user turns still replay their images
	// on the current message. Kiro history entries cannot carry images, so
	// without replay an image is only visible on the turn it arrives. Counted in
	// turns rather than images so a set attached together expires together. Zero
	// disables replay; negative means unlimited.
	HistoryImageTurns int
	// WebSearch translates Anthropic's WebSearch server tool into the web_search
	// tool AWS hosts behind the Kiro subscription, which kirocc then executes on
	// the model's behalf. Anthropic's schema-less server tools are stripped from
	// the request either way — Kiro rejects them — so disabling this only gives
	// up the replacement capability, never request validity.
	WebSearch bool
	// WebSearchFetch is how many top search-result pages are downloaded and
	// their readable text attached to the results the model sees. Zero
	// disables enrichment (snippets only).
	WebSearchFetch int
	// WebSearchFetchBytes caps the attached text per fetched page.
	WebSearchFetchBytes int
	// WebSearchVisible emits executed searches to the client as
	// server_tool_use / web_search_tool_result blocks (Anthropic's native
	// shape), so Claude Code renders them and replays the results in later
	// turns. False keeps searches invisible to the client.
	WebSearchVisible bool
	// ModelDiscovery enables fetching Kiro's model catalog at startup so newly
	// launched models resolve without a kirocc release. Built-in mappings always
	// win; discovery only fills gaps.
	ModelDiscovery    bool
	Debug             bool
	OTel              bool
	OTelBodyLimit     int
	KeepAliveInterval time.Duration
	LogFile           logging.LogFileConfig
}

// regionPattern matches the region forms Kiro uses ("us-east-1",
// "us-gov-west-1"). The value is interpolated into an API hostname, so it is
// validated rather than trusted — a stray "/" or "@" would otherwise send
// requests, and the bearer token with them, to an arbitrary host.
var regionPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// maxRegionLen bounds the region string; real regions are far shorter.
const maxRegionLen = 64

// DefaultDBPath returns the default kiro-cli SQLite database location.
func DefaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return DefaultDBPathFor(runtime.GOOS, home)
}

// DefaultDBPathFor returns the default database location for the given OS and home directory.
func DefaultDBPathFor(goos, home string) string {
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "kiro-cli", "data.sqlite3")
	default:
		return filepath.Join(home, ".local", "share", "kiro-cli", "data.sqlite3")
	}
}

// ApplyEnvOverrides mutates cfg using KIROCC_* environment variables.
func ApplyEnvOverrides(cfg *Config) error {
	applyString("KIROCC_DB_PATH", &cfg.DBPath)
	applyString("KIROCC_API_KEY", &cfg.APIKey)
	// Kiro's own variable names, so a machine already set up for headless
	// kiro-cli needs no kirocc-specific configuration.
	applyString("KIRO_API_KEY", &cfg.KiroAPIKey)
	applyString("KIROCC_HOST", &cfg.Host)
	applyString("KIRO_API_REGION", &cfg.Region)
	applyString("KIROCC_REGION", &cfg.Region)
	applyString("KIROCC_BASE_URL", &cfg.BaseURL)
	if err := applyInt64("KIROCC_MAX_BODY_SIZE", &cfg.MaxBodySize); err != nil {
		return err
	}
	if err := applyInt("KIROCC_HISTORY_IMAGE_TURNS", &cfg.HistoryImageTurns); err != nil {
		return err
	}
	if err := applyInt("KIROCC_PORT", &cfg.Port); err != nil {
		return err
	}
	if err := applyBool("KIROCC_WEB_SEARCH", &cfg.WebSearch); err != nil {
		return err
	}
	if err := applyInt("KIROCC_WEB_SEARCH_FETCH", &cfg.WebSearchFetch); err != nil {
		return err
	}
	if err := applyInt("KIROCC_WEB_SEARCH_FETCH_BYTES", &cfg.WebSearchFetchBytes); err != nil {
		return err
	}
	if err := applyBool("KIROCC_WEB_SEARCH_VISIBLE", &cfg.WebSearchVisible); err != nil {
		return err
	}
	if err := applyBool("KIROCC_MODEL_DISCOVERY", &cfg.ModelDiscovery); err != nil {
		return err
	}
	if err := applyBool("KIROCC_DEBUG", &cfg.Debug); err != nil {
		return err
	}
	if err := applyBool("KIROCC_OTEL", &cfg.OTel); err != nil {
		return err
	}
	if err := applyInt("KIROCC_OTEL_BODY_LIMIT", &cfg.OTelBodyLimit); err != nil {
		return err
	}
	if err := applyDuration("KIROCC_KEEPALIVE_INTERVAL", &cfg.KeepAliveInterval); err != nil {
		return err
	}
	applyString("KIROCC_LOG_FILE", &cfg.LogFile.Path)
	if err := applyInt("KIROCC_LOG_MAX_SIZE", &cfg.LogFile.MaxSize); err != nil {
		return err
	}
	if err := applyInt("KIROCC_LOG_MAX_BACKUPS", &cfg.LogFile.MaxBackups); err != nil {
		return err
	}
	if err := applyInt("KIROCC_LOG_MAX_AGE", &cfg.LogFile.MaxAge); err != nil {
		return err
	}
	if err := applyBool("KIROCC_LOG_COMPRESS", &cfg.LogFile.Compress); err != nil {
		return err
	}
	if err := applyBool("KIROCC_LOG_CONSOLE", &cfg.LogFile.Console); err != nil {
		return err
	}
	return nil
}

// Validate checks that the config is internally consistent. Returns an error
// describing the first violation found. Called after flag parsing and env
// overrides, before the server starts.
func (c *Config) Validate() error {
	if c.Host == "" {
		return fmt.Errorf("host must not be empty")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("port must be in 1..65535, got %d", c.Port)
	}
	if c.OTelBodyLimit < 0 {
		return fmt.Errorf("otel-body-limit must be >= 0, got %d", c.OTelBodyLimit)
	}
	if c.BaseURL != "" {
		u, err := url.Parse(c.BaseURL)
		if err != nil {
			return fmt.Errorf("invalid base-url %q: %w", c.BaseURL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("base-url must use http or https scheme, got %q", c.BaseURL)
		}
		if u.Host == "" {
			return fmt.Errorf("base-url must include a host, got %q", c.BaseURL)
		}
	}
	if c.Region != "" {
		if len(c.Region) > maxRegionLen || !regionPattern.MatchString(c.Region) {
			return fmt.Errorf("region must be a lowercase region like us-east-1, got %q", c.Region)
		}
	}
	if c.KeepAliveInterval != 0 && c.KeepAliveInterval < time.Second {
		return fmt.Errorf("keepalive-interval must be 0 or >= 1s, got %s", c.KeepAliveInterval)
	}
	return nil
}

func applyString(key string, dst *string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func applyInt(key string, dst *int) error {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid %s=%q: %w", key, v, err)
		}
		*dst = n
	}
	return nil
}

func applyInt64(key string, dst *int64) error {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid %s=%q: %w", key, v, err)
		}
		*dst = n
	}
	return nil
}

func applyBool(key string, dst *bool) error {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid %s=%q: %w", key, v, err)
		}
		*dst = b
	}
	return nil
}

func applyDuration(key string, dst *time.Duration) error {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid %s=%q: %w", key, v, err)
		}
		*dst = d
	}
	return nil
}
