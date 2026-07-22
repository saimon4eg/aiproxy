package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/whtsky/copilot2api/amplocal"
	"github.com/whtsky/copilot2api/ampsearch"
	"github.com/whtsky/copilot2api/anthropic"
	"github.com/whtsky/copilot2api/auth"
	"github.com/whtsky/copilot2api/gemini"
	"github.com/whtsky/copilot2api/internal/copilot"
	"github.com/whtsky/copilot2api/internal/models"
	"github.com/whtsky/copilot2api/internal/upstream"
	"github.com/whtsky/copilot2api/providers"
	"github.com/whtsky/copilot2api/providers/adapters"
	"github.com/whtsky/copilot2api/providers/openai"
	"github.com/whtsky/copilot2api/proxy"
	"github.com/whtsky/copilot2api/requestlog"
)

var version = "dev"

var activeProcesses sync.Map

var linguafrancaAvailable int32 // atomic: 0=unchecked, 1=available, -1=unavailable

func main() {
	var (
		port        = flag.Int("port", 0, "Server port (env: COPILOT2API_PORT, default: 7777)")
		host        = flag.String("host", "", "Server host (env: COPILOT2API_HOST, default: 127.0.0.1)")
		tokenDir    = flag.String("token-dir", "", "Token storage directory (env: COPILOT2API_TOKEN_DIR, default: ~/.config/copilot2api)")
		showVersion = flag.Bool("version", false, "Show version and exit")
		debug       = flag.Bool("debug", false, "Enable debug logging (env: COPILOT2API_DEBUG)")
	)
	flag.Parse()

	if !*debug {
		if v := os.Getenv("COPILOT2API_DEBUG"); v != "" {
			if enabled, err := strconv.ParseBool(v); err == nil {
				*debug = enabled
			}
		}
	}
	if *host == "" {
		if v := os.Getenv("COPILOT2API_HOST"); v != "" {
			*host = v
		} else {
			*host = "127.0.0.1"
		}
	}
	if *port == 0 {
		if v := os.Getenv("COPILOT2API_PORT"); v != "" {
			if p, err := strconv.Atoi(v); err == nil {
				*port = p
			}
		}
		if *port == 0 {
			*port = 7777
		}
	}
	if *tokenDir == "" {
		if v := os.Getenv("COPILOT2API_TOKEN_DIR"); v != "" {
			*tokenDir = v
		}
	}
	if *showVersion {
		fmt.Printf("copilot2api version %s\n", version)
		os.Exit(0)
	}

	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	if *debug {
		requestlog.Init("logs")
	}

	if *tokenDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			slog.Error("failed to get home directory", "error", err)
			os.Exit(1)
		}
		*tokenDir = filepath.Join(homeDir, ".config", "copilot2api")
	}

	authClient, err := auth.NewClient(*tokenDir)
	if err != nil {
		slog.Error("failed to initialize auth client", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()

	transport := upstream.NewTransport()
	wrappedTransport := requestlog.WrapTransport(transport)
	upstreamClient := upstream.NewClient(authClient, wrappedTransport, *debug)
	modelsCache := models.NewCache(upstreamClient, 5*time.Minute)

	// Copilot handler — fallback for copilot-* models and unmatched routes.
	proxyHandler := proxy.NewHandler(authClient, wrappedTransport, modelsCache, *debug)
	anthropicHandler := anthropic.NewHandler(authClient, wrappedTransport, modelsCache, *debug)
	geminiHandler := gemini.NewHandler(authClient, wrappedTransport, modelsCache, *debug)

	// Providers generalized router — all model routing goes through providers.json.
	cfg, err := providers.LoadConfig("providers.json", proxyHandler)
	if err != nil {
		slog.Error("failed to load providers.json", "error", err)
		os.Exit(1)
	}
	// copilot-* on /v1/messages routes through the Anthropic handler
	// (Anthropic→Responses conversion), matching upstream copilot2api.
	cfg.SetCopilotAnthropicHandler(anthropicHandler)

	// Copilot config (integration-id + proxy) from providers.json — applied
	// BEFORE authenticating so the device flow / token endpoints route through
	// the proxy, and upstream requests identify with the right integrator
	// (gates model access, e.g. 1M "-1m" variants).
	if cp := cfg.ByID("copilot"); cp != nil {
		copilot.SetIntegrationID(cp.IntegrationID)
		copilot.SetAPIVersion(cp.APIVersion)
		if cp.ProxyHost != "" {
			upstream.SetTransportProxy(transport, cp.ProxyHost)
			auth.SetProxy(cp.ProxyHost)
		}
	}

	// Copilot GitHub OAuth / Copilot token flow (uses the proxy set above).
	// Skip if no copilot provider is enabled.
	if cfg.IsCopilotEnabled() {
		if err := authClient.EnsureAuthenticated(ctx); err != nil {
			slog.Error("copilot authentication failed", "error", err)
			os.Exit(1)
		}
	} else {
		slog.Info("copilot disabled — skipping copilot authentication")
	}

	go cfg.InitOAuth(func(providerID string, proxyHost string) (providers.TokenProvider, error) {
		switch providerID {
		case "openai":
			oc := openai.NewConfig(proxyHost)
			if err := oc.LoadOrAuthenticate(); err != nil {
				return nil, fmt.Errorf("openai oauth: %w", err)
			}
			return oc, nil
		case "anthropic":
			ac := anthropic.NewConfigFromEnv(proxyHost)
			if err := ac.LoadOrAuthenticate(); err != nil {
				return nil, fmt.Errorf("anthropic oauth: %w", err)
			}
			return ac.ToNativeConfig(), nil
		}
		return nil, fmt.Errorf("unknown oauth provider: %s", providerID)
	})

	providers.RegisterAdapter("messages[deepseek]", adapters.NewDeepSeekAdapter())
	providersCache := providers.NewModelsCache(cfg)

	// Build mux.
	mux := http.NewServeMux()

	// Providers routes — generalized router.
	mux.Handle("/v1/models", providersCache)
	mux.Handle("/v1/messages", cfg)
	mux.Handle("/v1/chat/completions", cfg)
	mux.Handle("/v1/responses", cfg)
	mux.Handle("/v1/embeddings", proxyHandler)

	// Gemini.
	mux.Handle("/v1beta/models", geminiHandler)
	mux.Handle("/v1beta/models/", geminiHandler)

	// Usage and health.
	mux.HandleFunc("/usage", proxyHandler.HandleUsage)

	// Check linguafranca once at startup.
	if _, err := exec.LookPath("python3"); err == nil {
		cmd := exec.Command("python3", "-c", "import linguafranca")
		if cmd.Run() == nil {
			atomic.StoreInt32(&linguafrancaAvailable, 1)
		} else {
			atomic.StoreInt32(&linguafrancaAvailable, -1)
		}
	}
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		health := map[string]interface{}{
			"status":    "ok",
			"providers": len(cfg.Providers),
		}
		v := atomic.LoadInt32(&linguafrancaAvailable)
		health["linguafranca"] = v == 1
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(health)
	})

	// AmpCode routes.
	mux.Handle("/amp/v1/chat/completions", http.StripPrefix("/amp", proxyHandler))
	mux.Handle("/amp/v1/models", http.StripPrefix("/amp", proxyHandler))
	mux.Handle("/amp/v1/responses", http.StripPrefix("/amp", proxyHandler))
	mux.Handle("/amp/v1/embeddings", http.StripPrefix("/amp", proxyHandler))
	mux.Handle("/api/provider/openai/v1/chat/completions", http.StripPrefix("/api/provider/openai", proxyHandler))
	mux.Handle("/api/provider/openai/v1/responses", http.StripPrefix("/api/provider/openai", proxyHandler))
	mux.Handle("/api/provider/openai/v1/models", http.StripPrefix("/api/provider/openai", proxyHandler))
	mux.Handle("/api/provider/anthropic/v1/messages", http.StripPrefix("/api/provider/anthropic", anthropicHandler))
	mux.Handle("/api/provider/google/v1beta/models", http.StripPrefix("/api/provider/google", geminiHandler))
	mux.Handle("/api/provider/google/v1beta/models/", http.StripPrefix("/api/provider/google", geminiHandler))

	ampBackend, _ := url.Parse("https://ampcode.com")
	ampReverseProxy := newAmpReverseProxy(ampBackend)
	searchHandler := ampsearch.NewHandler(ampsearch.NewModelBackend(upstreamClient, ""))
	ampThreadsDir := os.Getenv("AMP_THREADS_DIR")
	if ampThreadsDir == "" {
		homeDir, _ := os.UserHomeDir()
		ampThreadsDir = filepath.Join(homeDir, ".config", "copilot2api", "threads")
	}
	slog.Info("amp local mode enabled", "threads_dir", ampThreadsDir)
	localState := amplocal.NewState(ampThreadsDir)
	localHandler := amplocal.NewHandler(localState)

	mux.HandleFunc("/api/internal", func(w http.ResponseWriter, r *http.Request) {
		if searchHandler.TryServe(w, r) { return }
		if localHandler.TryServeInternal(w, r) { return }
		ampReverseProxy.ServeHTTP(w, r)
	})
	mux.HandleFunc("/api/threads/find", localHandler.ServeThreadsFind)
	mux.HandleFunc("/api/threads/", localHandler.ServeThreadMarkdown)
	mux.HandleFunc("/api/telemetry", localHandler.ServeTelemetry)
	mux.HandleFunc("/api/durable-thread-workers/", localHandler.ServeDurableThreadWorker)
	mux.HandleFunc("/api/users/", localHandler.ServeUsers)
	mux.HandleFunc("/api/attachments", localHandler.ServeAttachments)
	mux.HandleFunc("/news.rss", localHandler.ServeNewsRSS)
	mux.Handle("/api/", ampReverseProxy)
	mux.HandleFunc("/amp/v1/login", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://ampcode.com/login", http.StatusFound)
	})
	mux.HandleFunc("/amp/auth/cli-login", func(w http.ResponseWriter, r *http.Request) {
		target := "https://ampcode.com/auth/cli-login"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusFound)
	})

	if cfg.IsCopilotEnabled() {
		go func() {
			slog.Debug("warming copilot models cache")
			modelsCache.Warm(ctx)
			slog.Info("copilot models cache warmed")
		}()
	} else {
		slog.Info("copilot disabled — skipping models cache warm")
	}

	// Plain HTTP on localhost. If deploying to a network, use a reverse proxy with TLS
	// (nginx, Caddy) in front of aiproxy.
	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", *host, *port),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		Handler:           requestIDMiddleware(corsMiddleware(rateLimitMiddleware(debugLogMiddleware(latencyMiddleware(logAllRequests(mux)))))),
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("starting server", "host", *host, "port", *port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-quit:
	case err := <-serverErr:
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}

	slog.Info("shutting down server")
	activeProcesses.Range(func(key, value interface{}) bool {
		if p, ok := value.(*os.Process); ok {
			p.Kill()
		}
		return true
	})

	ctxShutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(ctxShutdown); err != nil {
		slog.Error("server forced to shutdown", "error", err)
		os.Exit(1)
	}
	slog.Info("server stopped")
}

func newAmpReverseProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			slog.Debug("amp proxy", "method", req.Method, "path", req.URL.Path, "query", req.URL.RawQuery)
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("amp reverse proxy error", "error", err, "path", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "amp_proxy_error",
				"message": err.Error(),
			})
		},
	}
}

func logAllRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("incoming request", "method", r.Method, "path", r.URL.Path, "query", r.URL.RawQuery)
		next.ServeHTTP(w, r)
	})
}
