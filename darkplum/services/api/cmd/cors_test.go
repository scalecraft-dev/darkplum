package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// A browser on the web UI is same-origin through nginx whatever address it
// used, provided nginx forwards the Host header WITH its port ($http_host, not
// $host): the CORS middleware treats Origin == http(s)://Host as same-origin.
// Found on an AWS install: with $host a login from a port-forward on :18080
// reached the api as Host "localhost" and was a 403 before the password was
// checked. Any other Origin stays refused.
func TestCORS_SameOriginThroughProxyAllowedForeignRefused(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("CORS_ALLOWED_ORIGINS", "")
	r := gin.New()
	r.Use(corsMiddleware())
	r.POST("/auth/login", func(c *gin.Context) { c.Status(http.StatusOK) })

	cases := []struct {
		name, host, proto, origin string
		want                      int
	}{
		{"port-forward on a non-default port", "localhost:18080", "", "http://localhost:18080", http.StatusOK},
		{"published address over https", "darkplum.internal.example", "https", "https://darkplum.internal.example", http.StatusOK},
		{"default allowlist entry", "localhost:9999", "", "http://localhost:8080", http.StatusOK},
		{"no Origin (curl, SDK)", "localhost:18080", "", "", http.StatusOK},
		{"foreign site", "localhost:18080", "", "https://evil.example", http.StatusForbidden},
		{"same host, other port", "localhost:18080", "", "http://localhost:18081", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
			req.Host = tc.host
			if tc.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}
