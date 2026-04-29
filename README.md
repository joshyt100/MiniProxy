# MiniProxy

A lightweight reverse proxy in Go that multiplexes HTTP/1.1 and HTTP/2 (including gRPC) traffic on the same port.

## Features

- **HTTP/1.1, HTTP/2, and gRPC** — accepts h2c and HTTP/2 over TLS from clients, automatically detects gRPC via `Content-Type: application/grpc` and uses the correct upstream transport
- **Round-robin and least-connections load balancing**
- **Active and passive health checking** — see [Health checking](#health-checking)
- **TLS and cleartext listeners** — can run both simultaneously on separate ports
- **Prometheus metrics** — request counts, active connections, and duration histograms per upstream

## Quick start

```bash
git clone https://github.com/joshyt100/MiniProxy
cd MiniProxy
cp config.example.yaml config.yaml
# edit config.yaml with your upstreams
go run .
```

Or with Docker:

```bash
docker build -t miniproxy .
docker run -p 8080:8080 -p 8443:8443 -v $(pwd)/config.yaml:/app/config.yaml miniproxy
```

## Demo

Spins up two nginx backends, a gRPC echo server, Prometheus, and Grafana:

```bash
docker compose -f docker-compose.demo.yml up
```

| Service    | URL                           |
|------------|-------------------------------|
| Proxy      | http://localhost:8080         |
| Proxy TLS  | https://localhost:8443        |
| Metrics    | http://localhost:2112/metrics |
| Prometheus | http://localhost:2112         |
| Grafana    | http://localhost:3001         |

Test HTTP:
```bash
curl http://localhost:8080
```

Test gRPC:
```bash
grpcurl -plaintext -d '{"message":"hello"}' localhost:8080 echo.EchoService/Echo
```

## Configuration

```yaml
cleartext:
  enabled: true
  listen_addr: ":8080"
algo: rr
upstreams:
  - https://localhost:50051
  - http://localhost:50052
tls:
  enabled: true
  listen_addr: ":8443"
  cert: "certs/cert.pem"
  key: "certs/key.pem"
metrics:
  enabled: true
  listen_addr: ":2112"
rate_limit:
  enabled: false
  rps: 10
  burst: 5
  per_ip: true
health:
  enabled: false
  path: /healthz
  interval_seconds: 10
  timeout_seconds: 2
  passive_cooldown_secs: 30
server:
  read_timeout: 30s
  write_timeout: 30s
  idle_timeout: 120s
  max_concurrent_streams: 250
```

### Options

| Field | Description | Default |
|-------|-------------|---------|
| `cleartext.enabled` | Enable HTTP/h2c listener | `true` |
| `cleartext.listen_addr` | Address to listen on | `:8080` |
| `tls.enabled` | Enable HTTPS/HTTP2 listener | `false` |
| `tls.listen_addr` | TLS address to listen on | `:8443` |
| `tls.cert` | Path to TLS certificate | |
| `tls.key` | Path to TLS private key | |
| `algo` | Load balancing algorithm (`rr` or `lc`) | `lc` |
| `upstreams` | List of upstream URLs | |
| `metrics.enabled` | Enable Prometheus metrics endpoint | `false` |
| `metrics.listen_addr` | Address to expose metrics on | `:2112` |
| `rate_limit.enabled` | Enable rate limiting | `false` |
| `rate_limit.rps` | Requests per second allowed | `10` |
| `rate_limit.burst` | Burst size above RPS limit | `5` |
| `rate_limit.per_ip` | Apply rate limit per IP address | `true` |
| `health.enabled` | Enable active health checks | `false` |
| `health.path` | HTTP path to probe on each upstream | `/healthz` |
| `health.interval_seconds` | How often to probe each upstream (seconds) | `10` |
| `health.timeout_seconds` | Probe timeout before marking upstream unhealthy (seconds) | `2` |
| `health.passive_cooldown_secs` | How long to remove a failing upstream from rotation (seconds) | `30` |
| `server.read_timeout` | Max time to read an incoming request | `30s` |
| `server.write_timeout` | Max time to write a response | `30s` |
| `server.idle_timeout` | Max time to keep an idle connection open | `120s` |
| `server.max_concurrent_streams` | Max concurrent HTTP/2 streams | `250` |

## Health checking

**Active** — a background goroutine polls each upstream at a fixed interval with a GET request. If the upstream returns a non-2xx/3xx response or the request fails, it is marked unhealthy and skipped by the balancer. It is re-admitted automatically once a subsequent poll succeeds.

**Passive** — when a proxied request to an upstream fails at the transport level (connection refused, timeout, etc.), that upstream is marked unhealthy for a fixed cooldown period. It is re-admitted automatically after the cooldown expires and active health checks pass.

Upstreams are never removed from the pool — they stay in the list but are skipped while unhealthy.

## Metrics

Prometheus metrics are exposed at `/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `proxy_requests_total` | Counter | Total requests by upstream and status code |
| `proxy_active_connections` | Gauge | In-flight requests per upstream |
| `proxy_request_duration_seconds` | Histogram | Request duration per upstream |
| `proxy_upstream_healthy` | Gauge | Health status per upstream (1 = healthy, 0 = unhealthy) |

## Building

```bash
go build -o miniproxy .
```


## License

This project is licensed under the MIT License.
