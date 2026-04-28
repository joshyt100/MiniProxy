package proxy

import (
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"reverse-proxy/health"
	"reverse-proxy/metrics"
	"strconv"
	"strings"
	"sync"
	"time"
)

var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Proxy-Connection":    {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

type Proxy struct {
	upstreams      []*url.URL
	upstreamIndex  map[string]int
	http11Client   *http.Client
	h2cClient      *http.Client
	h2tlsClient    *http.Client
	balancer       Balancer
	health         *health.State
	baseTransport  *http.Transport
	logger         *slog.Logger
	metricsEnabled bool
}

// New constructs and returns a fully initialised Proxy from the provided Options.
// It sets up the HTTP/1.1, h2c, and h2-over-TLS clients, starts the upstream
// health checker, and wires the configured load-balancing algorithm.
func New(opts Options) *Proxy {
	http11Transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          4096,
		MaxIdleConnsPerHost:   1024,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 500 * time.Millisecond,
		ResponseHeaderTimeout: 10 * time.Second,
		DisableCompression:    true,

		// Keep this disabled for HTTP/1.1 upstream benchmarks.
		// gRPC upstreams use the explicit h2c/h2 transports below.
		TLSNextProto: make(map[string]func(string, *tls.Conn) http.RoundTripper),
	}

	baseTransport := opts.Transport
	if baseTransport == nil {
		baseTransport = http11Transport
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	upstreamIndex := make(map[string]int, len(opts.Upstreams))
	for i, up := range opts.Upstreams {
		upstreamIndex[up.String()] = i
	}

	p := &Proxy{
		upstreams:      opts.Upstreams,
		upstreamIndex:  upstreamIndex,
		http11Client:   &http.Client{Transport: http11Transport},
		h2cClient:      &http.Client{Transport: newH2CTransport()},
		h2tlsClient:    &http.Client{Transport: newH2cTLSTransport()},
		baseTransport:  baseTransport,
		logger:         logger,
		metricsEnabled: opts.MetricsEnabled,
	}

	hs := health.NewState(
		p.upstreams,
		baseTransport,
		opts.HealthPath,
		opts.HealthInterval,
		opts.HealthTimeout,
		opts.PassiveFailCooldown,
	)
	p.health = hs
	p.health.Start()

	algo := opts.Algo
	if algo == "" {
		algo = LBLeastConn
	}

	switch algo {
	case LBRoundRobin:
		p.balancer = newRoundRobinBalancer(p.upstreams, p.health)
	default:
		p.balancer = newLeastConnBalancer(p.upstreams, p.health)
	}

	return p
}

// ParseUpstreams parses a comma-separated list of upstream URLs and returns
// them as a slice of *url.URL. Each entry must include a scheme (http or https)
// and a host; an error is returned for any malformed or unsupported value.
func ParseUpstreams(csv string) ([]*url.URL, error) {
	parts := strings.Split(csv, ",")
	out := make([]*url.URL, 0, len(parts))

	for _, raw := range parts {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}

		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid upstream %q: %w", raw, err)
		}

		if u.Host == "" {
			return nil, fmt.Errorf("upstream must include scheme and host, got %q", raw)
		}

		switch strings.ToLower(u.Scheme) {
		case "http", "https":
		default:
			return nil, fmt.Errorf("upstream scheme must be http or https, got %q", u.Scheme)
		}

		u.Path = strings.TrimRight(u.Path, "/")
		out = append(out, u)
	}

	return out, nil
}

// ServeHTTP implements http.Handler. For each inbound request it picks an
// upstream via the balancer, forwards the request, and streams the response
// back to the client. If an upstream fails it is marked unhealthy and the next
// available upstream is tried, up to len(upstreams) attempts.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	grpc := isGRPC(r)

	if p.logger.Enabled(r.Context(), slog.LevelDebug) {
		p.logger.Debug("incoming request",
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
			"grpc", grpc,
		)
	}

	for attempt := range len(p.upstreams) {
		up, done, ok := p.balancer.Pick(r)
		if !ok {
			p.logger.Error("no upstreams available",
				"method", r.Method,
				"path", r.URL.Path,
			)
			http.Error(w, "no upstreams available", http.StatusBadGateway)
			return
		}

		upLabel := up.Host

		if p.metricsEnabled {
			metrics.ActiveConnections.WithLabelValues(upLabel).Inc()
		}

		outReq, err := p.buildUpstreamRequest(r, up, grpc)
		if err != nil {
			done()

			if p.metricsEnabled {
				metrics.ActiveConnections.WithLabelValues(upLabel).Dec()
			}

			p.logger.Warn("failed to build upstream request",
				"upstream", up.Host,
				"attempt", attempt,
				"error", err,
			)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}

		resp, err := p.clientFor(grpc, up).Do(outReq)
		if err != nil {
			p.markUpstreamFailure(up)
			done()

			if p.metricsEnabled {
				metrics.ActiveConnections.WithLabelValues(upLabel).Dec()
				metrics.RequestsTotal.WithLabelValues(upLabel, "502").Inc()
				metrics.RequestDuration.WithLabelValues(upLabel).Observe(time.Since(start).Seconds())
			}

			p.logger.Warn("upstream request failed, retrying",
				"upstream", up.String(),
				"attempt", attempt,
				"grpc", grpc,
				"scheme", up.Scheme,
				"error", err,
			)
			continue
		}

		if p.metricsEnabled {
			metrics.RequestsTotal.WithLabelValues(upLabel, strconv.Itoa(resp.StatusCode)).Inc()
		}

		copyHeaders(w.Header(), resp.Header)
		removeHopByHopHeaders(w.Header())

		w.WriteHeader(resp.StatusCode)

		// Important for gRPC: send headers immediately, then flush each streamed chunk.
		if grpc {
			flush(w)
		}

		copyErr := copyResponse(w, resp.Body, grpc)
		closeErr := resp.Body.Close()

		for k, vv := range resp.Trailer {
			for _, v := range vv {
				w.Header().Set(http.TrailerPrefix+k, v)
			}
		}

		if p.metricsEnabled {
			metrics.ActiveConnections.WithLabelValues(upLabel).Dec()
			metrics.RequestDuration.WithLabelValues(upLabel).Observe(time.Since(start).Seconds())
		}

		done()

		if copyErr != nil && p.logger.Enabled(r.Context(), slog.LevelDebug) {
			p.logger.Debug("response copy ended with error", "error", copyErr)
		}

		if closeErr != nil && p.logger.Enabled(r.Context(), slog.LevelDebug) {
			p.logger.Debug("error closing response body", "error", closeErr)
		}

		return
	}

	http.Error(w, "bad gateway", http.StatusBadGateway)
}

// clientFor returns the appropriate HTTP client for the given upstream.
// gRPC traffic uses HTTP/2; non-gRPC traffic uses HTTP/1.1.
// For gRPC, h2-over-TLS is chosen when the upstream scheme is https,
// otherwise the h2c (cleartext HTTP/2) client is used.
func (p *Proxy) clientFor(grpc bool, up *url.URL) *http.Client {
	if !grpc {
		return p.http11Client
	}

	if strings.EqualFold(up.Scheme, "https") {
		return p.h2tlsClient
	}

	return p.h2cClient
}

// markUpstreamFailure records a passive health failure for the given upstream
// so the balancer can deprioritise or temporarily remove it from rotation.
// It is a no-op when health checking is disabled.
func (p *Proxy) markUpstreamFailure(up *url.URL) {
	if p.health == nil {
		return
	}

	if i, ok := p.upstreamIndex[up.String()]; ok {
		p.health.MarkPassiveFailure(i)
	}
}

// buildUpstreamRequest constructs the outbound *http.Request that will be sent
// to the chosen upstream. It rewrites the URL, forwards the inbound context
// (so cancellation propagates), sanitises headers, and sets X-Forwarded-* fields.
// For gRPC the request body is streamed through a pipe to avoid buffering;
// regular HTTP requests forward the body reader directly.
func (p *Proxy) buildUpstreamRequest(in *http.Request, up *url.URL, grpc bool) (*http.Request, error) {
	target := *up
	target.Path = joinURLPath(up.Path, in.URL.Path)
	target.RawQuery = in.URL.RawQuery

	var body io.Reader

	if grpc {
		// gRPC needs streaming request-body forwarding.
		// Do NOT replace this with buffering, or streaming/reflection can hang.
		pr, pw := io.Pipe()
		go func() {
			_, err := io.Copy(pw, in.Body)
			_ = pw.CloseWithError(err)
		}()
		body = pr
	} else if hasRequestBody(in) {
		// Fast path for normal HTTP: no goroutine, no pipe.
		body = in.Body
	}

	outReq, err := http.NewRequestWithContext(in.Context(), in.Method, target.String(), body)
	if err != nil {
		if pr, ok := body.(*io.PipeReader); ok {
			_ = pr.CloseWithError(err)
		}
		return nil, err
	}

	if body == nil {
		outReq.ContentLength = 0
	} else {
		outReq.ContentLength = in.ContentLength
	}
	outReq.GetBody = nil

	copyHeaders(outReq.Header, in.Header)
	removeHopByHopHeaders(outReq.Header)

	outReq.Host = up.Host

	addXForwardedFor(outReq.Header, in)
	addXForwardedProto(outReq.Header, in)
	addXForwardedHost(outReq.Header, in)

	if grpc {
		outReq.Header.Set("Te", "trailers")
	} else {
		preserveOnlyTrailersTE(outReq.Header)
	}

	return outReq, nil
}

// copyResponse streams the upstream response body into the ResponseWriter.
// For gRPC each read chunk is flushed immediately to preserve streaming semantics.
// For regular HTTP a pooled buffer is used for efficient bulk copying.
func copyResponse(w http.ResponseWriter, body io.Reader, grpc bool) error {
	bufPtr := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufPtr)

	if !grpc {
		_, err := io.CopyBuffer(w, body, *bufPtr)
		return err
	}

	for {
		n, err := body.Read(*bufPtr)
		if n > 0 {
			if _, writeErr := w.Write((*bufPtr)[:n]); writeErr != nil {
				return writeErr
			}
			flush(w)
		}

		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// flush calls Flush on the ResponseWriter if it implements http.Flusher,
// pushing any buffered data to the client immediately.
func flush(w http.ResponseWriter) {
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

// hasRequestBody reports whether the inbound request carries a body that should
// be forwarded. GET and HEAD requests are treated as body-less regardless of
// whether a body is technically present.
func hasRequestBody(r *http.Request) bool {
	if r.Body == nil || r.Body == http.NoBody {
		return false
	}

	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return false
	}

	return true
}

// copyHeaders copies all headers from src into dst, canonicalising header names
// in the process so casing is normalised.
func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		dst[textproto.CanonicalMIMEHeaderKey(k)] = vv
	}
}

// removeHopByHopHeaders removes headers that must not be forwarded between hops,
// including any headers listed in the Connection header itself.
func removeHopByHopHeaders(h http.Header) {
	if c := h.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			if name := strings.TrimSpace(f); name != "" {
				h.Del(name)
			}
		}
	}

	for name := range hopByHopHeaders {
		delete(h, name)
	}
}

// addXForwardedFor appends the client's IP address to the X-Forwarded-For header,
// preserving any existing chain of addresses set by upstream proxies.
func addXForwardedFor(h http.Header, in *http.Request) {
	ip := clientIP(in)
	if ip == "" {
		return
	}

	if prior := h.Get("X-Forwarded-For"); prior != "" {
		h.Set("X-Forwarded-For", prior+", "+ip)
	} else {
		h.Set("X-Forwarded-For", ip)
	}
}

// addXForwardedProto sets the X-Forwarded-Proto header to "https" when the
// inbound connection was made over TLS, and "http" otherwise.
func addXForwardedProto(h http.Header, in *http.Request) {
	proto := "http"
	if in.TLS != nil {
		proto = "https"
	}
	h.Set("X-Forwarded-Proto", proto)
}

// addXForwardedHost sets the X-Forwarded-Host header to the Host value from
// the original inbound request, allowing the upstream to know the originally
// requested host.
func addXForwardedHost(h http.Header, in *http.Request) {
	if in.Host != "" {
		h.Set("X-Forwarded-Host", in.Host)
	}
}

// clientIP extracts the client's IP address from the request's RemoteAddr,
// stripping the port if present. Returns an empty string if the address
// cannot be parsed.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}

	if net.ParseIP(r.RemoteAddr) != nil {
		return r.RemoteAddr
	}

	return ""
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

// isGRPC reports whether the inbound request is a gRPC call by checking
// that the Content-Type header begins with "application/grpc".
func isGRPC(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}
