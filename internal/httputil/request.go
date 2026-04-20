package httputil

import (
	"net/http"
	"strings"
)

// ExtractAPIKey extracts an API key from the request.
// Checks: Authorization: Bearer <key>, x-api-key header, ?key= query param.
func ExtractAPIKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	if key := r.Header.Get("x-api-key"); key != "" {
		return key
	}
	return r.URL.Query().Get("key")
}

// Paginate extracts offset and limit from query parameters with defaults.
func Paginate(r *http.Request) (offset, limit int) {
	offset = QueryInt(r, "offset", 0)
	limit = QueryInt(r, "limit", 20)
	if limit > 100 {
		limit = 100
	}
	return
}

// QueryInt parses an integer query parameter with a default value.
func QueryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	var n int
	for _, c := range v {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}
