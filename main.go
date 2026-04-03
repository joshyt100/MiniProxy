package main

import (
	"crypto/tls"
	"flag"
	"log"
	"net/http"
	"reverse-proxy/config"
	"reverse-proxy/metrics"
	"reverse-proxy/proxy"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	metrics.Register()

	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatal(err)
	}

	listen := flag.String("listen", cfg.ListenAddr, "listen address")
	upstreamsCSV := flag.String("upstreams", "", "comma-separated upstream URLs (overrides config)")
	flag.Parse()

	cfg.ListenAddr = *listen
	if *upstreamsCSV != "" {
		cfg.Upstreams = strings.Split(*upstreamsCSV, ",")
	}

	if cfg.TLS.Enabled {
		if cfg.TLS.CertFile == "" || cfg.TLS.KeyFile == "" {
			log.Fatal("tls.cert and tls.key are required when tls.enabled is true")
		}
		if _, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil {
			log.Fatalf("failed to load TLS cert/key: %v", err)
		}
	}

	upstreams, err := proxy.ParseUpstreams(strings.Join(cfg.Upstreams, ","))
	if err != nil {
		log.Fatal(err)
	}
	if len(upstreams) == 0 {
		log.Fatal("no upstreams provided")
	}

	p := proxy.New(proxy.Options{
		Upstreams:           upstreams,
		Algo:                proxy.LBAlgo(cfg.Algo),
		HealthPath:          "/health",
		HealthInterval:      10 * time.Second,
		HealthTimeout:       2 * time.Second,
		PassiveFailCooldown: 30 * time.Second,
	})

	// metrics on separate internal port
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		log.Println("metrics listening on :9090")
		log.Fatal(http.ListenAndServe(":9090", mux))
	}()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
	}

	if cfg.TLS.Enabled {
		srv.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			CurvePreferences: []tls.CurveID{
				tls.X25519,
				tls.CurveP256,
			},

			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			},
		}
		log.Printf("TLS enabled, listening on %s -> %v", cfg.ListenAddr, upstreams)
		log.Fatal(srv.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile))
	} else {
		log.Printf("listening on %s -> %v", cfg.ListenAddr, upstreams)
		log.Fatal(srv.ListenAndServe())
	}
}
