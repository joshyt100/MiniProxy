package proxy

import (
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

type LBAlgo string

const (
	LBRoundRobin LBAlgo = "rr"
	LBLeastConn  LBAlgo = "lc"
)

type Options struct {
	Upstreams []*url.URL
	Transport *http.Transport
	Algo      LBAlgo
	Logger    *slog.Logger

	HealthPath          string
	HealthInterval      time.Duration
	HealthTimeout       time.Duration
	PassiveFailCooldown time.Duration
}
