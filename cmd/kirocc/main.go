package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/d-kuro/kirocc/internal/app/messages"
	"github.com/d-kuro/kirocc/internal/auth"
	"github.com/d-kuro/kirocc/internal/config"
	"github.com/d-kuro/kirocc/internal/kirocatalog"
	"github.com/d-kuro/kirocc/internal/kiroclient"
	"github.com/d-kuro/kirocc/internal/kiromcp"
	"github.com/d-kuro/kirocc/internal/logging"
	"github.com/d-kuro/kirocc/internal/models"
	"github.com/d-kuro/kirocc/internal/reqconv"
	"github.com/d-kuro/kirocc/internal/server"
	"github.com/d-kuro/kirocc/internal/tokencount"
	"github.com/d-kuro/kirocc/internal/tracing"
	"github.com/d-kuro/kirocc/internal/webfetch"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	cfg, err := parseFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if err := config.ApplyEnvOverrides(&cfg); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logHandler, logCloser := logging.NewHandler(cfg.Debug, cfg.LogFile)
	slog.SetDefault(slog.New(logHandler))
	if cfg.LogFile.Path != "" {
		slog.Info("file logging enabled", "path", cfg.LogFile.Path)
	}

	var otelShutdown func(context.Context) error
	if cfg.OTel {
		shutdown, err := tracing.Init(ctx)
		if err != nil {
			return fmt.Errorf("otel init: %w", err)
		}
		otelShutdown = shutdown
		slog.Info("OpenTelemetry tracing enabled", "body_limit", cfg.OTelBodyLimit)
	}

	authMgr := auth.NewAuthManager(cfg.DBPath, auth.WithRegionOverride(cfg.Region))
	if cfg.Region != "" {
		slog.Info("Kiro API region overridden", "region", cfg.Region)
	}
	kiroClient := buildKiroClient(authMgr, cfg)
	srv := buildServer(authMgr, kiroClient, cfg)

	if cfg.ModelDiscovery {
		go discoverModels(ctx, authMgr, cfg.Region)
	}

	// Eagerly initialize tiktoken so the first API request doesn't block on BPE data fetch.
	go tokencount.Preload()

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	if cfg.APIKey == "" && !isLoopback(cfg.Host) {
		slog.Warn("server is binding to a non-loopback address without an API key — all endpoints are unauthenticated",
			"host", cfg.Host)
	}
	slog.Info("kirocc listening", "addr", "http://"+addr)
	slog.Info("set ANTHROPIC_BASE_URL to use with Claude Code", "url", "http://"+addr)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout is intentionally not set: this server streams SSE responses
		// that can last minutes. A fixed WriteTimeout would kill long-running streams.
		// Slowloris is mitigated by ReadHeaderTimeout on the request side.
	}

	done := awaitShutdown(httpSrv, otelShutdown, logCloser)

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server: %w", err)
	}
	<-done
	return nil
}

func parseFlags(args []string) (config.Config, error) {
	fs := flag.NewFlagSet("kirocc", flag.ContinueOnError)
	var cfg config.Config
	fs.IntVar(&cfg.Port, "port", 3456, "listen port")
	fs.StringVar(&cfg.Host, "host", "127.0.0.1", "bind host")
	fs.StringVar(&cfg.DBPath, "db", config.DefaultDBPath(), "kiro-cli SQLite DB path")
	fs.StringVar(&cfg.APIKey, "api-key", "", "optional API key for authentication")
	fs.StringVar(&cfg.Region, "region", "", "override the Kiro API region (default: resolved from credentials)")
	fs.StringVar(&cfg.Region, "kiro-api-region", "", "alias for -region; also KIRO_API_REGION")
	fs.StringVar(&cfg.BaseURL, "base-url", "", "override the Kiro runtime endpoint entirely (default: https://runtime.<region>.kiro.dev/)")
	fs.Int64Var(&cfg.MaxBodySize, "max-body-size", messages.DefaultMaxBodySize, "max accepted client request body in bytes (0 = unlimited)")
	fs.IntVar(&cfg.HistoryImageTurns, "history-image-turns", reqconv.DefaultHistoryImageTurns, "how many earlier user turns still replay their images on the current message, since Kiro history cannot carry them (0 = disable replay, negative = unlimited)")
	fs.BoolVar(&cfg.WebSearch, "web-search", true, "translate Anthropic's WebSearch server tool into the Kiro-hosted web_search tool and execute it locally (schema-less Anthropic server tools are stripped either way)")
	fs.IntVar(&cfg.WebSearchFetch, "web-search-fetch", 3, "download this many top search-result pages and attach their readable text to the results (0 = snippets only)")
	fs.IntVar(&cfg.WebSearchFetchBytes, "web-search-fetch-bytes", 6144, "max bytes of attached page text per fetched result")
	fs.BoolVar(&cfg.WebSearchVisible, "web-search-visible", true, "stream executed searches to the client as server_tool_use/web_search_tool_result blocks so they render in Claude Code and persist across turns")
	fs.BoolVar(&cfg.ModelDiscovery, "model-discovery", true, "fetch Kiro's model catalog at startup so new models resolve without a kirocc update; also KIROCC_MODEL_DISCOVERY")
	fs.BoolVar(&cfg.Debug, "debug", false, "enable debug logging with OTel JSON Lines output")
	fs.BoolVar(&cfg.OTel, "otel", false, "enable OpenTelemetry tracing (OTLP HTTP exporter)")
	fs.IntVar(&cfg.OTelBodyLimit, "otel-body-limit", config.DefaultOTelBodyLimit, "max bytes of request body to capture in OTel spans (0 = unlimited)")
	fs.StringVar(&cfg.LogFile.Path, "log-file", "", "write logs to file with rotation (for agent debugging)")
	fs.IntVar(&cfg.LogFile.MaxSize, "log-max-size", logging.DefaultLogMaxSize, "max log file size in MB before rotation")
	fs.IntVar(&cfg.LogFile.MaxBackups, "log-max-backups", logging.DefaultLogMaxBackups, "max number of old log files to retain")
	fs.IntVar(&cfg.LogFile.MaxAge, "log-max-age", logging.DefaultLogMaxAge, "max days to retain old log files")
	fs.BoolVar(&cfg.LogFile.Compress, "log-compress", false, "compress rotated log files with gzip")
	fs.BoolVar(&cfg.LogFile.Console, "log-console", false, "also write logs to console when -log-file is set")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func buildKiroClient(authMgr *auth.AuthManager, cfg config.Config) kiroclient.Client {
	clientOpts := []kiroclient.HTTPClientOption{
		kiroclient.WithTokenCounter(tokencount.CountBytes),
		kiroclient.WithTokenRefresher(func(ctx context.Context) (string, error) {
			// Invalidate cache so GetToken re-reads from DB and refreshes
			// instead of returning the same rejected token.
			authMgr.InvalidateCache()
			creds, err := authMgr.GetToken(ctx)
			if err != nil {
				return "", err
			}
			return creds.AccessToken, nil
		}),
	}
	if cfg.BaseURL != "" {
		clientOpts = append(clientOpts, kiroclient.WithBaseURL(cfg.BaseURL))
		slog.Info("Kiro runtime endpoint overridden", "base_url", cfg.BaseURL)
	}
	if cfg.Region != "" {
		clientOpts = append(clientOpts, kiroclient.WithRegion(cfg.Region))
	}
	if cfg.OTel {
		clientOpts = append(clientOpts, kiroclient.WithOTel(cfg.OTelBodyLimit))
	}
	return kiroclient.NewHTTPClient(clientOpts...)
}

// modelDiscoveryTimeout bounds the startup catalog fetch. Generous, because it
// runs in the background and a slow answer costs nothing.
const modelDiscoveryTimeout = 20 * time.Second

// discoverModels installs Kiro's advertised model catalog as a fallback layer
// behind the built-in mapping table, so a model Kiro launches after this build
// resolves with the right context window and effort enum instead of falling
// through to pass-through defaults. Best-effort: every failure path leaves the
// built-in tables untouched.
//
// It runs in the background because it needs a credential, and blocking startup
// on a network round trip would delay the listener for no benefit — the built-in
// table already covers every model shipped with this build.
func discoverModels(ctx context.Context, authMgr *auth.AuthManager, regionOverride string) {
	ctx, cancel := context.WithTimeout(ctx, modelDiscoveryTimeout)
	defer cancel()

	creds, err := authMgr.GetToken(ctx)
	if err != nil {
		slog.Debug("model discovery skipped: no credentials", "err", err)
		return
	}
	if creds.ProfileARN == "" {
		// The catalog API rejects a request without a profile ARN. Nothing to do
		// but keep the built-in table.
		slog.Debug("model discovery skipped: credential has no profile ARN", "auth_type", creds.AuthType)
		return
	}
	region := creds.Region
	if regionOverride != "" {
		region = regionOverride
	}

	catalog, err := kirocatalog.New().List(ctx, kirocatalog.Request{
		Token:      creds.AccessToken,
		Region:     region,
		ProfileARN: creds.ProfileARN,
	})
	if err != nil {
		slog.Warn("model discovery failed, using built-in model table", "region", region, "err", err)
		return
	}

	entries := make([]models.CatalogModel, 0, len(catalog))
	for _, m := range catalog {
		entries = append(entries, models.CatalogModel{
			ID:             m.ID,
			MaxInputTokens: m.MaxInputTokens,
			EffortEnum:     m.EffortEnum,
		})
	}
	added := models.SetCatalog(entries)
	slog.Info("model catalog discovered",
		"region", region, "advertised", len(catalog), "new_models", added)
}

func buildServer(authMgr *auth.AuthManager, client kiroclient.Client, cfg config.Config) *server.Server {
	var opts []server.ServerOption
	if cfg.OTel {
		opts = append(opts, server.WithOTel(cfg.OTelBodyLimit))
	}
	if cfg.Debug {
		opts = append(opts, server.WithCapture(true))
	}
	opts = append(opts, server.WithMaxBodySize(cfg.MaxBodySize))
	opts = append(opts, server.WithHistoryImageTurns(cfg.HistoryImageTurns))
	if cfg.WebSearch {
		opts = append(opts, server.WithMCPClient(buildMCPClient(authMgr)))
		if cfg.WebSearchFetch > 0 {
			opts = append(opts, server.WithWebFetch(webfetch.New(), cfg.WebSearchFetch, cfg.WebSearchFetchBytes))
		}
		opts = append(opts, server.WithWebSearchVisible(cfg.WebSearchVisible))
	}
	return server.New(authMgr, cfg.APIKey, client, opts...)
}

// buildMCPClient constructs the client for the MCP endpoint AWS hosts behind
// the Kiro subscription. It shares the credential store with the runtime
// client, including the same refresh-on-403 path.
func buildMCPClient(authMgr *auth.AuthManager) *kiromcp.HTTPClient {
	return kiromcp.NewHTTPClient(
		kiromcp.WithTokenRefresher(func(ctx context.Context) (string, error) {
			authMgr.InvalidateCache()
			creds, err := authMgr.GetToken(ctx)
			if err != nil {
				return "", err
			}
			return creds.AccessToken, nil
		}),
	)
}

func isLoopback(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// awaitShutdown registers a SIGINT/SIGTERM handler that gracefully stops the
// HTTP server, flushes OTel spans, and closes the log file. Returns a channel
// that closes when shutdown is complete.
func awaitShutdown(httpSrv *http.Server, otelShutdown func(context.Context) error, logCloser interface{ Close() error }) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		slog.Info("shutting down", "signal", sig.String())

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			slog.Error("shutdown error", "err", err)
		}
		if otelShutdown != nil {
			if err := otelShutdown(ctx); err != nil {
				slog.Error("otel shutdown error", "err", err)
			}
		}
		if err := logCloser.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "log close error: %v\n", err)
		}
		close(done)
	}()
	return done
}
