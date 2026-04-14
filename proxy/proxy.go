package proxy

import (
	"crypto/tls"
	"fmt"
	"io"
	// "log/slog"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	// "os"
	"reverse-proxy/health"
	"reverse-proxy/metrics"
	"strconv"
	"strings"
	"time"
	// "golang.org/x/net/http2"
)

type Proxy struct {
	upstreams     []*url.URL
	http11Client  *http.Client // HTTP/1.1 only — nginx, plain HTTP upstreams
	h2cClient     *http.Client // h2c — gRPC upstreams
	h2tlsClient   *http.Client //h2 - gRPC over TLS (https://)
	balancer      Balancer
	health        *health.State
	baseTransport *http.Transport
}

func New(opts Options) *Proxy {
	http11Transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
	}

	baseTransport := opts.Transport
	if baseTransport == nil {
		baseTransport = http11Transport
	}

	p := &Proxy{
		upstreams:     opts.Upstreams,
		http11Client:  &http.Client{Transport: http11Transport},
		h2cClient:     &http.Client{Transport: newH2CTransport()},
		h2tlsClient:   &http.Client{Transport: newH2cTLSTransport()},
		baseTransport: baseTransport,
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
			// valid
		default:
			return nil, fmt.Errorf("upstream scheme must be http or https, got %q", u.Scheme)
		}
		u.Path = strings.TrimRight(u.Path, "/")
		out = append(out, u)
	}
	return out, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	grpc := isGRPC(r)

	for attempt := 0; attempt < len(p.upstreams); attempt++ {
		up, done, ok := p.balancer.Pick(r)
		if !ok {
			http.Error(w, "no upstreams available", http.StatusBadGateway)
			return
		}

		upLabel := up.Host
		metrics.ActiveConnections.WithLabelValues(upLabel).Inc()

		outReq, err := p.buildUpstreamRequest(r, up, grpc)
		if err != nil {
			done()
			metrics.ActiveConnections.WithLabelValues(upLabel).Dec()
			// Non-retryable — bad request construction.
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}

		resp, err := p.clientFor(grpc, up).Do(outReq)
		if err != nil {
			p.markUpstreamFailure(up)
			done()
			metrics.ActiveConnections.WithLabelValues(upLabel).Dec()
			metrics.RequestsTotal.WithLabelValues(upLabel, "502").Inc()
			metrics.RequestDuration.WithLabelValues(upLabel).Observe(time.Since(start).Seconds())
			// Retryable — try next upstream.
			continue
		}

		// Success path.
		resp.Body = &doneReadCloser{
			rc: resp.Body,
			done: func() {
				metrics.ActiveConnections.WithLabelValues(upLabel).Dec()
				metrics.RequestDuration.WithLabelValues(upLabel).Observe(time.Since(start).Seconds())
				done()
			},
		}
		defer func() { _ = resp.Body.Close() }()

		metrics.RequestsTotal.WithLabelValues(upLabel, strconv.Itoa(resp.StatusCode)).Inc()

		copyHeaders(w.Header(), resp.Header)
		removeHopByHopHeaders(w.Header())
		w.WriteHeader(resp.StatusCode)

		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}

		buf := make([]byte, 32*1024)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if _, writeErr := w.Write(buf[:n]); writeErr != nil {
					break
				}
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
			}
			if readErr != nil {
				break
			}
		}

		for k, vv := range resp.Trailer {
			for _, v := range vv {
				w.Header().Set(http.TrailerPrefix+k, v)
			}
		}
		return
	}

	// All upstreams tried and failed.
	http.Error(w, "bad gateway", http.StatusBadGateway)
}

// clientFor picks the right HTTP client for the upstream.
//
//	non-gRPC              → http11Client  (HTTP/1.1, TLSNextProto disabled)
//	gRPC + http://        → h2cClient     (HTTP/2 cleartext / h2c)
//	gRPC + https://       → h2tlsClient   (HTTP/2 over TLS, ALPN h2)
func (p *Proxy) clientFor(grpc bool, up *url.URL) *http.Client {
	if !grpc {
		return p.http11Client
	}

	if strings.EqualFold(up.Scheme, "https") {
		return p.h2tlsClient
	}
	return p.h2cClient
}

func (p *Proxy) markUpstreamFailure(up *url.URL) {
	if p.health == nil {
		return
	}
	for i := range p.upstreams {
		if p.upstreams[i].String() == up.String() {
			p.health.MarkPassiveFailure(i)
			return
		}
	}
}

func (p *Proxy) buildUpstreamRequest(in *http.Request, up *url.URL, grpc bool) (*http.Request, error) {
	target := *up
	target.Path = joinURLPath(up.Path, in.URL.Path)
	target.RawQuery = in.URL.RawQuery

	pr, pw := io.Pipe()
	go func() {
		_, err := io.Copy(pw, in.Body)
		pw.CloseWithError(err)
	}()

	outReq, err := http.NewRequestWithContext(in.Context(), in.Method, target.String(), pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return nil, err
	}

	outReq.ContentLength = -1
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

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		k = textproto.CanonicalMIMEHeaderKey(k)
		dst.Del(k)
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func removeHopByHopHeaders(h http.Header) {
	// RFC 7230 §6.1 — remove headers listed in Connection first.
	if c := h.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			if name := strings.TrimSpace(f); name != "" {
				h.Del(name)
			}
		}
	}

	h.Del("Connection")
	h.Del("Proxy-Connection")
	h.Del("Keep-Alive")
	h.Del("Proxy-Authenticate")
	h.Del("Proxy-Authorization")
	h.Del("Trailer")
	h.Del("Transfer-Encoding")
	h.Del("Upgrade")
}

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

func addXForwardedProto(h http.Header, in *http.Request) {
	proto := "http"
	if in.TLS != nil {
		proto = "https"
	}
	h.Set("X-Forwarded-Proto", proto)
}

func addXForwardedHost(h http.Header, in *http.Request) {
	if in.Host != "" {
		h.Set("X-Forwarded-Host", in.Host)
	}
}

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

// isGRPC reports whether the incoming request is a gRPC call.
// The gRPC spec mandates Content-Type starting with "application/grpc"
// (e.g. "application/grpc", "application/grpc+proto", "application/grpc+json").
func isGRPC(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc")
}
