package httputil

import "net/http"

// CORSMiddleware adds CORS headers to all responses.
// allowHeaders defaults to "*" if empty.
func CORSMiddleware(next http.Handler, allowHeaders ...string) http.Handler {
	headers := "*"
	if len(allowHeaders) > 0 && allowHeaders[0] != "" {
		headers = allowHeaders[0]
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", headers)
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
