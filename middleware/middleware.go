package middleware

import "net/http"

type Middleware func(http.Handler) http.Handler

// Chain wraps h with the given middlewares in order, so the first middleware
// is the outermost — it runs first on the way in and last on the way out.
// Example: Chain(proxy, rateLimiter, logging) produces:
//
//	request → rateLimiter → logging → proxy → response
func Chain(h http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}
