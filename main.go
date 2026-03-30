package main

import (
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

	upstreams, err := proxy.ParseUpstreams(strings.Join(cfg.Upstreams, ","))
	if err != nil {
		log.Fatal(err)
	}
	if len(upstreams) == 0 {
		log.Fatal("no upstreams provided")
	}

	p := proxy.New(proxy.Options{
		Upstreams:           upstreams,
		HealthPath:          "/health",
		HealthInterval:      10 * time.Second,
		HealthTimeout:       2 * time.Second,
		PassiveFailCooldown: 30 * time.Second,
	})

	// expose /metrics on a separate internal port so it's not proxied
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
	log.Printf("listening on %s -> %v", cfg.ListenAddr, upstreams)
	log.Fatal(srv.ListenAndServe())
}
