package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// resolveSecret returns a secret value from the value itself, or a file
// (systemd-creds sets *_FILE env vars to a credential path), or empty string.
func resolveSecret(value, fileFlag string) (string, error) {
	if value != "" {
		return value, nil
	}
	if fileFlag != "" {
		data, err := os.ReadFile(fileFlag)
		if err != nil {
			return "", fmt.Errorf("reading %q: %w", fileFlag, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return "", nil
}

func main() {
	configPath := flag.String("config", "", "path to ollama-hook.yaml config file")
	debug := flag.Bool("debug", false, "shortcut for --log-level=debug")
	logLevelFlag := flag.String("log-level", "", "log level: debug, info, warn, error (overrides config)")
	logFormatFlag := flag.String("log-format", "", "log format: text or json (overrides config)")
	doMigrate := flag.Bool("migrate", false, "run database migrations and exit")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	if *logLevelFlag != "" {
		cfg.LogLevel = *logLevelFlag
	}
	if *logFormatFlag != "" {
		cfg.LogFormat = *logFormatFlag
	}
	if *debug {
		cfg.LogLevel = "debug"
	}

	logger := NewLogger(cfg.LogLevel, cfg.LogFormat)

	// --migrate: apply pending migrations and exit. Independent of whether
	// the db plugin is enabled, so it reads the DSN straight from config.
	if *doMigrate {
		dsn, err := resolveSecret(cfg.Plugins.DB.URL, cfg.Plugins.DB.URLFile)
		if err != nil {
			logger.Error("failed to resolve database URL", "error", err)
			os.Exit(1)
		}
		if dsn == "" {
			logger.Error("--migrate requires plugins.db.url (DATABASE_URL) or plugins.db.url_file (DATABASE_URL_FILE) in config")
			os.Exit(1)
		}
		if err := RunMigrations(dsn); err != nil {
			logger.Error("migration failed", "error", err)
			os.Exit(1)
		}
		v, dirty, err := MigrationVersion(dsn)
		if err != nil {
			logger.Error("failed to read migration version", "error", err)
			os.Exit(1)
		}
		logger.Info("migrations applied", "version", v, "dirty", dirty)
		return
	}

	upstream, err := url.Parse(cfg.Upstream)
	if err != nil {
		log.Fatalf("invalid upstream %q: %v", cfg.Upstream, err)
	}

	// Always-on hooks, then whatever plugins opted in via config.
	metricsHook := NewMetricsHook(logger)
	hooks := []Hook{NewLogHook(logger), metricsHook}

	pluginHooks, err := BuildPlugins(cfg, logger)
	if err != nil {
		logger.Error("failed to initialize plugins", "error", err)
		os.Exit(1)
	}
	hooks = append(hooks, pluginHooks...)

	hook := NewMultiHook(hooks...)
	handler := NewProxyHandler(upstream, hook, metricsHook, logger)

	// Any plugin hook that also implements ExternalLogger backs POST /_proxy/log.
	for _, h := range pluginHooks {
		if el, ok := h.(ExternalLogger); ok {
			handler.externalLogger = el
			break
		}
	}

	if t := envOrDefault("PROXY_LOG_TOKEN", ""); t != "" {
		handler.logToken = t
	} else if f := envOrDefault("PROXY_LOG_TOKEN_FILE", ""); f != "" {
		data, err := os.ReadFile(f)
		if err != nil {
			logger.Warn("could not read PROXY_LOG_TOKEN_FILE", "error", err)
		} else {
			handler.logToken = strings.TrimSpace(string(data))
		}
	}

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: handler,
	}

	// Graceful shutdown on SIGTERM / SIGINT.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-stop
		logger.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	logger.Info("starting proxy",
		"listen", cfg.Listen,
		"upstream", cfg.Upstream,
		"plugins_enabled", len(pluginHooks),
	)
	fmt.Fprintf(os.Stderr, "ollama-hook listening on %s → %s\n", cfg.Listen, cfg.Upstream)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}

	// Flush/close any plugin hooks that hold resources (DB connections,
	// TCP sockets, ...) after the server stops accepting requests.
	closePlugins(pluginHooks, logger)
}

// closer is satisfied by any hook that holds resources needing cleanup on
// shutdown (e.g. *DatabaseHook, *RGBHook). Hooks that don't need it simply
// don't implement it.
type closer interface {
	Close() error
}

func closePlugins(hooks []Hook, logger *slog.Logger) {
	for _, h := range hooks {
		c, ok := h.(closer)
		if !ok {
			continue
		}
		if err := c.Close(); err != nil {
			logger.Warn("error closing plugin", "error", err)
		}
	}
}
