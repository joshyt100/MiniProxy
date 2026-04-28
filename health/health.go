package health

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"reverse-proxy/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// State tracks the health of a set of upstream servers using two complementary
// mechanisms: active checks (periodic HTTP pings) and passive checks (failures
// observed by the proxy during live traffic). Both must agree an upstream is
// healthy before it is eligible to receive requests.
type State struct {
	// ups is the fixed list of upstream URLs being monitored.
	// Indices here correspond directly to indices in healthy and passiveUntil.
	ups []*url.URL

	// healthy holds the result of the most recent active health check for each
	// upstream. An upstream starts healthy and is updated by checkAllOnce.
	// atomic.Bool is used so the health-check goroutine can write concurrently
	// with the proxy goroutines that read it.
	healthy []atomic.Bool

	// passiveUntil holds a Unix nanosecond timestamp per upstream. If the
	// current time is before this value, the upstream is considered passively
	// unhealthy regardless of what the active check says. Set by
	// MarkPassiveFailure when live traffic observes a failure, and cleared
	// automatically when the next active check passes.
	passiveUntil []atomic.Int64

	// healthPath is the HTTP path hit on each upstream during active checks,
	// e.g. "/healthz". An empty path disables active checking entirely.
	healthPath string

	// interval is how often checkAllOnce is called by the background goroutine.
	interval time.Duration

	// timeout is the per-request deadline applied to each individual health
	// check HTTP call.
	timeout time.Duration

	// passiveCooldown is how long an upstream stays passively unhealthy after
	// MarkPassiveFailure is called. After this window, it becomes eligible
	// again unless the active check also considers it down.
	passiveCooldown time.Duration

	// client is the HTTP client used exclusively for active health check
	// requests. It shares the same transport as the proxy so connection
	// behaviour is consistent, but is kept separate to avoid interfering
	// with proxy traffic.
	client *http.Client

	// stopCh is closed by Stop to signal the background goroutine to exit.
	stopCh chan struct{}
}

// NewState constructs a State for the given upstreams. All upstreams begin
// healthy. Active health checks are started separately by calling Start.
//
// tr is the HTTP transport to use for health check requests — typically the
// same base transport as the proxy. healthPath is the path to GET on each
// upstream (e.g. "/healthz"); an empty string disables active checks.
// interval controls how often checks run, timeout caps each individual request,
// and passiveCooldown sets how long an upstream is penalised after a passive
// failure is observed by the proxy.
func NewState(ups []*url.URL, tr http.RoundTripper, healthPath string, interval, timeout, passiveCooldown time.Duration) *State {
	s := &State{
		ups:             ups,
		healthy:         make([]atomic.Bool, len(ups)),
		passiveUntil:    make([]atomic.Int64, len(ups)),
		healthPath:      healthPath,
		interval:        interval,
		timeout:         timeout,
		passiveCooldown: passiveCooldown,
		client:          &http.Client{Transport: tr},
		stopCh:          make(chan struct{}),
	}
	for i := range ups {
		s.healthy[i].Store(true)
	}
	return s
}

// Start launches the background goroutine that runs active health checks on
// every upstream at the configured interval. It performs an immediate check
// before the first tick so the proxy does not start with stale state.
//
// Start is a no-op if the State is nil, or if interval, timeout, or healthPath
// are not configured — in which case active checking is disabled and upstreams
// are considered healthy unless passive failures say otherwise.
func (s *State) Start() {
	if s == nil || s.interval <= 0 || s.timeout <= 0 || s.healthPath == "" {
		log.Printf("[health] not starting: interval=%v timeout=%v path=%q", s.interval, s.timeout, s.healthPath)
		return
	}
	log.Printf("[health] starting checks every %v against path %q", s.interval, s.healthPath)
	go func() {
		t := time.NewTicker(s.interval)
		defer t.Stop()
		s.checkAllOnce()
		for {
			select {
			case <-t.C:
				s.checkAllOnce()
			case <-s.stopCh:
				return
			}
		}
	}()
}

// AnyHealthy reports whether at least one upstream is currently healthy.
// It is used by the balancer to decide whether to bother attempting to pick
// an upstream at all. Returns true when s is nil so a missing health state
// does not incorrectly block traffic.
func (s *State) AnyHealthy() bool {
	if s == nil {
		return true
	}
	now := time.Now().UnixNano()
	for i := range s.ups {
		if s.isHealthyAt(i, now) {
			return true
		}
	}
	return false
}

// IsHealthy reports whether the upstream at index i is currently healthy,
// considering both the active check result and any active passive penalty.
func (s *State) IsHealthy(i int) bool {
	return s.isHealthyAt(i, time.Now().UnixNano())
}

// MarkPassiveFailure records a passive health failure for upstream i, placing
// it in a cooldown window during which it will not receive new requests. The
// cooldown expires automatically after passiveCooldown, and is also cleared
// early if the next active check passes.
//
// This is called by the proxy whenever a live request to an upstream fails,
// allowing the balancer to react to failures faster than the active check
// interval would allow.
func (s *State) MarkPassiveFailure(i int) {
	if s == nil || s.passiveCooldown <= 0 {
		return
	}
	until := time.Now().Add(s.passiveCooldown).UnixNano()
	s.passiveUntil[i].Store(until)
}

// isHealthyAt is the internal implementation of health checking, accepting a
// pre-captured now timestamp so that AnyHealthy can call it in a loop without
// each iteration making its own time.Now() syscall.
//
// An upstream is considered healthy only when its active check has passed AND
// its passive cooldown window (if any) has expired.
func (s *State) isHealthyAt(i int, now int64) bool {
	if s == nil {
		return true
	}
	if !s.healthy[i].Load() {
		return false
	}
	until := s.passiveUntil[i].Load()
	if until != 0 && now < until {
		return false
	}
	return true
}

// checkAllOnce runs an active health check against every upstream concurrently,
// updating each upstream's healthy flag and the UpstreamHealthy Prometheus gauge
// with the result. It blocks until all checks complete.
func (s *State) checkAllOnce() {
	var wg sync.WaitGroup
	for i := range s.ups {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok := s.checkOne(i)
			s.healthy[i].Store(ok)
			val := 0.0
			if ok {
				val = 1.0
			}
			metrics.UpstreamHealthy.WithLabelValues(s.ups[i].Host).Set(val)
		}(i)
	}
	wg.Wait()
}

// checkOne performs a single active health check against upstream i by making
// a GET request to its configured health path. Any non-2xx/3xx response or
// request error is treated as a failure.
//
// On success, the passive penalty for this upstream is cleared so it becomes
// immediately eligible for traffic without waiting for the cooldown to expire.
func (s *State) checkOne(i int) bool {
	up := s.ups[i]
	target := *up
	target.Path = joinURLPath(up.Path, s.healthPath)
	target.RawQuery = ""
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	ok := resp.StatusCode >= 200 && resp.StatusCode < 400
	if ok {
		// Clear passive penalty on recovery so the upstream
		// is eligible immediately after active check passes.
		s.passiveUntil[i].Store(0)
	}
	return ok
}

// joinURLPath concatenates a base path and a request path, ensuring exactly
// one slash between them and that the result always begins with a slash.
func joinURLPath(a, b string) string {
	switch {
	case a == "" || a == "/":
		return cleanPath(b)
	case b == "" || b == "/":
		return cleanPath(a)
	default:
		return cleanPath(strings.TrimRight(a, "/") + "/" + strings.TrimLeft(b, "/"))
	}
}

// cleanPath ensures p begins with a leading slash, returning "/" for empty input.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// Stop signals the background health-check goroutine to exit by closing the
// stop channel. It should be called when the proxy is shutting down to avoid
// leaking goroutines.
func (s *State) Stop() {
	close(s.stopCh)
}
