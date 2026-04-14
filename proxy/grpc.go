package proxy

import (
	"crypto/tls"
	"net"
	"net/http"
	"strings"

	"golang.org/x/net/http2"
)

// func isH2CUpstream(uScheme string) bool {
// 	return strings.EqualFold(uScheme, "h2c")
// }

// newH2CTransport returns an http2.Transport that dials plain TCP.
// DialTLS is called even for cleartext h2c connections; we simply
// return a raw TCP conn and let the HTTP/2 framer take over.
func newH2CTransport() *http2.Transport {
	return &http2.Transport{
		AllowHTTP: true,
		DialTLS: func(network, addr string, _ *tls.Config) (net.Conn, error) {
			return net.Dial(network, addr)
		},
	}
}

// newH2TLSTransport returns an HTTP/2 transport that uses real TLS + ALPN.
// Used for gRPC-over-TLS upstreams (https:// scheme)
func newH2cTLSTransport() *http2.Transport {
	return &http2.Transport{}
}

// preserveOnlyTrailersTE scans all TE header values and retains only "trailers".
// If "trailers" is present (even in a comma-separated list), it normalizes the
// header to exactly "TE: trailers". Otherwise, the TE header is removed.
// This is required for HTTP/2/gRPC compliance and to strip hop-by-hop encodings.
func preserveOnlyTrailersTE(h http.Header) {
	values := h.Values("TE")
	if len(values) == 0 {
		return
	}

	keepTrailers := false
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "trailers") {
				keepTrailers = true
				break
			}
		}
		if keepTrailers {
			break
		}
	}

	if keepTrailers {
		h.Set("TE", "trailers")
	} else {
		h.Del("TE")
	}
}
