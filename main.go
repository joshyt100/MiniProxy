package main

import (
	"crypto/tls"
	"log/slog"
	"net/http"
	"os"
	"reverse-proxy/config"
	"reverse-proxy/metrics"
	"reverse-proxy/middleware"
	"reverse-proxy/proxy"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		slog.Error("failed to load config", "path", "config.yaml", "error", err)
		os.Exit(1)
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)

	logger.Info("config loaded",
		"cleartext_enabled", cfg.Cleartext.Enabled,
		"tls_enabled", cfg.TLS.Enabled,
		"metrics_enabled", cfg.Metrics.Enabled,
		"rate_limit_enabled", cfg.RateLimit.Enabled,
		"algo", cfg.Algo,
	)

	metrics.Register()
	logger.Info("metrics registered")

	upstreams, err := proxy.ParseUpstreams(joinUpstreams(cfg.Upstreams))
	if err != nil {
		logger.Error("failed to parse upstreams", "error", err)
		os.Exit(1)
	}

	algo := proxy.LBAlgo(cfg.Algo)
	if !cfg.Cleartext.Enabled && !cfg.TLS.Enabled {
		logger.Error("invalid configuration",
			"error", "at least one of cleartext or tls must be enabled",
		)
		os.Exit(1)
	}

	p := proxy.New(proxy.Options{
		Upstreams:           upstreams,
		Algo:                algo,
		HealthPath:          "/",
		HealthInterval:      5 * time.Second,
		HealthTimeout:       2 * time.Second,
		PassiveFailCooldown: 10 * time.Second,
		// Logger:              logger,
	})

	mux := http.NewServeMux()
	mux.Handle("/", p)

	if cfg.Metrics.Enabled {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", promhttp.Handler())

		go func() {
			logger.Info("metrics server listening",
				"addr", cfg.Metrics.ListenAddr,
			)

			if err := http.ListenAndServe(cfg.Metrics.ListenAddr, metricsMux); err != nil {
				logger.Error("metrics server failed",
					"addr", cfg.Metrics.ListenAddr,
					"error", err,
				)
				os.Exit(1)
			}
		}()
	}

	var handler http.Handler = mux
	if cfg.RateLimit.Enabled {
		rl := middleware.NewRateLimiter(cfg.RateLimit.RPS, cfg.RateLimit.Burst, cfg.RateLimit.PerIP)
		handler = middleware.Chain(handler, rl.Middleware())

		logger.Info("rate limiter enabled",
			"rps", cfg.RateLimit.RPS,
			"burst", cfg.RateLimit.Burst,
			"per_ip", cfg.RateLimit.PerIP,
		)
	}

	h2s := &http2.Server{
		MaxConcurrentStreams: 250,
	}
	h2cHandler := h2c.NewHandler(handler, h2s)

	if cfg.Cleartext.Enabled {
		clearSrv := &http.Server{
			Addr:    cfg.Cleartext.ListenAddr,
			Handler: h2cHandler,
		}

		if err := http2.ConfigureServer(clearSrv, h2s); err != nil {
			logger.Error("failed to configure cleartext http2 server",
				"addr", cfg.Cleartext.ListenAddr,
				"error", err,
			)
			os.Exit(1)
		}

		go func() {
			logger.Info("proxy cleartext server listening",
				"addr", cfg.Cleartext.ListenAddr,
				"protocols", "h2c,http/1.1",
			)

			if err := clearSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("cleartext server failed",
					"addr", cfg.Cleartext.ListenAddr,
					"error", err,
				)
				os.Exit(1)
			}
		}()
	}

	if cfg.TLS.Enabled {
		tlsSrv := &http.Server{
			Addr:    cfg.TLS.ListenAddr,
			Handler: handler,
			TLSConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
		}

		if err := http2.ConfigureServer(tlsSrv, h2s); err != nil {
			logger.Error("failed to configure tls http2 server",
				"addr", cfg.TLS.ListenAddr,
				"error", err,
			)
			os.Exit(1)
		}

		logger.Info("proxy tls server listening",
			"addr", cfg.TLS.ListenAddr,
			"protocols", "tls,http/2",
		)

		if err := tlsSrv.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil {
			logger.Error("tls server failed",
				"addr", cfg.TLS.ListenAddr,
				"cert_file", cfg.TLS.CertFile,
				"key_file", cfg.TLS.KeyFile,
				"error", err,
			)
			os.Exit(1)
		}
	} else {
		select {}
	}
}

func joinUpstreams(us []string) string {
	result := ""
	for i, u := range us {
		if i > 0 {
			result += ","
		}
		result += u
	}
	return result
}

func newLogger(cfg *config.Config) *slog.Logger {
	level := parseLogLevel(cfg.Logger.Level)

	opts := &slog.HandlerOptions{
		Level: level,
	}

	var handler slog.Handler
	switch strings.ToLower(cfg.Logger.Format) {
	case "text":
		handler = slog.NewTextHandler(os.Stdout, opts)
	default:
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}

func parseLogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
