package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	// "strings"
	"testing"
	"time"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("failed to parse url %q: %v", raw, err)
	}

	return u
}

func TestParseUpstreams_Valid(t *testing.T) {
	ups, err := ParseUpstreams("http://localhost:9000, http://localhost:9001/api")
	if err != nil {
		t.Fatalf("ParseUpstreams returned error: %v", err)
	}

	if len(ups) != 2 {
		t.Fatalf("expected 2 upstreams, got %d", len(ups))
	}

	if got := ups[0].String(); got != "http://localhost:9000" {
		t.Fatalf("unexpected first upstream: %s", got)
	}

	if got := ups[1].String(); got != "http://localhost:9001/api" {
		t.Fatalf("unexpected second upstream: %s", got)
	}
}

func TestParseUpstreams_Invalid(t *testing.T) {
	_, err := ParseUpstreams("localhost:9000")
	if err == nil {
		t.Fatal("expected error for upstream without scheme")
	}
}

func TestParseUpstreams_SkipsEmptyEntries(t *testing.T) {
	ups, err := ParseUpstreams("http://localhost:9000, , http://localhost:9001")
	if err != nil {
		t.Fatalf("ParseUpstreams returned error: %v", err)
	}

	if len(ups) != 2 {
		t.Fatalf("expected 2 upstreams, got %d", len(ups))
	}
}

func TestJoinURLPath(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want string
	}{
		{name: "root and path", a: "/", b: "/api", want: "/api"},
		{name: "base path and child", a: "/base", b: "/api", want: "/base/api"},
		{name: "trims duplicate slash", a: "/base/", b: "/api", want: "/base/api"},
		{name: "empty base", a: "", b: "api", want: "/api"},
		{name: "empty child", a: "/base", b: "", want: "/base"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := joinURLPath(tt.a, tt.b)
			if got != tt.want {
				t.Fatalf("joinURLPath(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestBuildUpstreamRequest(t *testing.T) {
	p := &Proxy{}

	in := httptest.NewRequest(http.MethodGet, "http://proxy.local/api/users?id=7", nil)
	in.Host = "proxy.local"
	in.RemoteAddr = "192.168.1.50:34567"
	in.Header.Set("Connection", "X-Custom-Hop")
	in.Header.Set("X-Custom-Hop", "remove-me")
	in.Header.Set("X-Test", "keep-me")

	up := mustURL(t, "http://upstream1:80/base")

	outReq, err := p.buildUpstreamRequest(in, up, false)
	if err != nil {
		t.Fatalf("buildUpstreamRequest returned error: %v", err)
	}

	if got := outReq.URL.String(); got != "http://upstream1:80/base/api/users?id=7" {
		t.Fatalf("unexpected upstream url: %s", got)
	}

	if outReq.Host != "upstream1:80" {
		t.Fatalf("expected host upstream1:80, got %s", outReq.Host)
	}

	if got := outReq.Header.Get("X-Test"); got != "keep-me" {
		t.Fatalf("expected X-Test header to be preserved, got %q", got)
	}

	if got := outReq.Header.Get("X-Custom-Hop"); got != "" {
		t.Fatalf("expected hop-by-hop header to be removed, got %q", got)
	}

	if got := outReq.Header.Get("Connection"); got != "" {
		t.Fatalf("expected Connection header to be removed, got %q", got)
	}

	if got := outReq.Header.Get("X-Forwarded-For"); got != "192.168.1.50" {
		t.Fatalf("unexpected X-Forwarded-For header: %q", got)
	}

	if got := outReq.Header.Get("X-Forwarded-Proto"); got != "http" {
		t.Fatalf("unexpected X-Forwarded-Proto header: %q", got)
	}

	if got := outReq.Header.Get("X-Forwarded-Host"); got != "proxy.local" {
		t.Fatalf("unexpected X-Forwarded-Host header: %q", got)
	}
}

func TestServeHTTP_ForwardsRequestAndResponse(t *testing.T) {
	upstreamCalled := false

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true

		if r.URL.Path != "/base/hello" {
			t.Fatalf("expected upstream path /base/hello, got %s", r.URL.Path)
		}

		if r.URL.RawQuery != "name=josh" {
			t.Fatalf("expected query name=josh, got %s", r.URL.RawQuery)
		}

		if got := r.Header.Get("X-Forwarded-Host"); got != "proxy.local" {
			t.Fatalf("unexpected X-Forwarded-Host: %q", got)
		}

		w.Header().Set("X-Upstream", "ok")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("forwarded"))
	}))
	defer upstream.Close()

	upURL := mustURL(t, upstream.URL+"/base")

	p := New(Options{
		Upstreams:           []*url.URL{upURL},
		HealthPath:          "/health",
		HealthInterval:      0,
		HealthTimeout:       0,
		PassiveFailCooldown: 0,
	})

	req := httptest.NewRequest(http.MethodGet, "http://proxy.local/hello?name=josh", nil)
	req.Host = "proxy.local"
	req.RemoteAddr = "127.0.0.1:45678"

	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	resp := rr.Result()
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed reading response body: %v", err)
	}

	if !upstreamCalled {
		t.Fatal("expected upstream to be called")
	}

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", resp.StatusCode)
	}

	if got := resp.Header.Get("X-Upstream"); got != "ok" {
		t.Fatalf("expected X-Upstream header to be copied, got %q", got)
	}

	if string(body) != "forwarded" {
		t.Fatalf("unexpected response body: %q", string(body))
	}
}

func TestServeHTTP_NoUpstreamsAvailable(t *testing.T) {
	p := New(Options{
		Upstreams:           []*url.URL{},
		HealthPath:          "/health",
		HealthInterval:      0,
		HealthTimeout:       0,
		PassiveFailCooldown: 0,
	})

	req := httptest.NewRequest(http.MethodGet, "http://proxy.local/test", nil)
	rr := httptest.NewRecorder()

	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502, got %d", rr.Code)
	}
}

func TestServeHTTP_UpstreamFailureReturnsBadGateway(t *testing.T) {
	badUpstream := mustURL(t, "http://127.0.0.1:1")

	p := New(Options{
		Upstreams:           []*url.URL{badUpstream},
		HealthPath:          "/health",
		HealthInterval:      0,
		HealthTimeout:       0,
		PassiveFailCooldown: 50 * time.Millisecond,
	})

	req := httptest.NewRequest(http.MethodGet, "http://proxy.local/test", nil)
	rr := httptest.NewRecorder()

	p.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502, got %d", rr.Code)
	}
}

func TestIsGRPC(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		want        bool
	}{
		{name: "grpc exact", contentType: "application/grpc", want: true},
		{name: "grpc proto suffix", contentType: "application/grpc+proto", want: true},
		{name: "grpc json suffix", contentType: "application/grpc+json", want: true},
		{name: "json", contentType: "application/json", want: false},
		{name: "empty", contentType: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://proxy.local/service.Method", nil)
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}

			got := isGRPC(req)
			if got != tt.want {
				t.Fatalf("isGRPC(%q) = %v, want %v", tt.contentType, got, tt.want)
			}
		})
	}
}

func TestPreserveOnlyTrailersTE_KeepsTrailers(t *testing.T) {
	h := http.Header{}
	h.Add("TE", "gzip, trailers")
	h.Add("TE", "deflate")

	preserveOnlyTrailersTE(h)

	if got := h.Get("TE"); got != "trailers" {
		t.Fatalf("expected TE=trailers, got %q", got)
	}
}

func TestPreserveOnlyTrailersTE_RemovesNonTrailers(t *testing.T) {
	h := http.Header{}
	h.Set("TE", "gzip")

	preserveOnlyTrailersTE(h)

	if got := h.Get("TE"); got != "" {
		t.Fatalf("expected TE header to be removed, got %q", got)
	}
}

func TestBuildUpstreamRequest_GRPCSetsTEAndPreservesContentType(t *testing.T) {
	p := &Proxy{}

	in := httptest.NewRequest(http.MethodPost, "http://proxy.local/echo.EchoService/Echo", nil)
	in.Host = "proxy.local"
	in.RemoteAddr = "10.0.0.5:34567"
	in.Header.Set("Content-Type", "application/grpc+proto")
	in.Header.Set("TE", "gzip, trailers")
	in.Header.Set("Connection", "TE")

	up := mustURL(t, "http://upstream1:8080")

	outReq, err := p.buildUpstreamRequest(in, up, true)
	if err != nil {
		t.Fatalf("buildUpstreamRequest returned error: %v", err)
	}

	if got := outReq.URL.String(); got != "http://upstream1:8080/echo.EchoService/Echo" {
		t.Fatalf("unexpected upstream url: %s", got)
	}

	if got := outReq.Header.Get("Content-Type"); got != "application/grpc+proto" {
		t.Fatalf("expected gRPC content-type to be preserved, got %q", got)
	}

	if got := outReq.Header.Get("Te"); got != "trailers" {
		t.Fatalf("expected Te=trailers for gRPC, got %q", got)
	}

	if got := outReq.Header.Get("Connection"); got != "" {
		t.Fatalf("expected Connection header to be removed, got %q", got)
	}

	if got := outReq.Header.Get("X-Forwarded-For"); got != "10.0.0.5" {
		t.Fatalf("unexpected X-Forwarded-For header: %q", got)
	}
}

func TestBuildUpstreamRequest_NonGRPCNormalizesTE(t *testing.T) {
	p := &Proxy{}

	in := httptest.NewRequest(http.MethodGet, "http://proxy.local/api/test", nil)
	in.Host = "proxy.local"
	in.RemoteAddr = "10.0.0.6:45678"
	in.Header.Set("TE", "gzip, trailers")
	in.Header.Set("Connection", "close")

	up := mustURL(t, "http://upstream1:8080")

	outReq, err := p.buildUpstreamRequest(in, up, false)
	if err != nil {
		t.Fatalf("buildUpstreamRequest returned error: %v", err)
	}

	if got := outReq.Header.Get("TE"); got != "trailers" {
		t.Fatalf("expected TE=trailers for non-gRPC normalization, got %q", got)
	}
}

func TestClientFor_GRPCUsesH2CClient(t *testing.T) {
	p := New(Options{
		Upstreams:           []*url.URL{mustURL(t, "http://localhost:9000")},
		HealthPath:          "/health",
		HealthInterval:      0,
		HealthTimeout:       0,
		PassiveFailCooldown: 0,
	})

	if got := p.clientFor(true); got != p.h2cClient {
		t.Fatal("expected gRPC requests to use h2cClient")
	}

	if got := p.clientFor(false); got != p.http11Client {
		t.Fatal("expected non-gRPC requests to use http11Client")
	}
}
