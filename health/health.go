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

type State struct {
	ups []*url.URL

	healthy      []atomic.Bool
	passiveUntil []atomic.Int64

	healthPath      string
	interval        time.Duration
	timeout         time.Duration
	passiveCooldown time.Duration

	client *http.Client
	stopCh chan struct{}
}

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

func (s *State) IsHealthy(i int) bool {
	return s.isHealthyAt(i, time.Now().UnixNano())
}

func (s *State) MarkPassiveFailure(i int) {
	if s == nil || s.passiveCooldown <= 0 {
		return
	}
	until := time.Now().Add(s.passiveCooldown).UnixNano()
	s.passiveUntil[i].Store(until)
}

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

func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}
