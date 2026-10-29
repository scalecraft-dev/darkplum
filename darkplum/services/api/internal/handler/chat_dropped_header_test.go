package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// The gateway's dropped-parameter header must reach an SDK user through the
// api's chat proxy (Bearer callers) and the webchat.
func TestCopyProxyHeaders_PassesDroppedParams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("X-Darkplum-Dropped-Params", "seed,max_tokens=raised")
	resp.Header.Set("X-Internal-Secret", "must-not-pass")
	copyProxyHeaders(c, resp)
	if got := w.Header().Get("X-Darkplum-Dropped-Params"); got != "seed,max_tokens=raised" {
		t.Errorf("X-Darkplum-Dropped-Params = %q", got)
	}
	if w.Header().Get("X-Internal-Secret") != "" {
		t.Error("an unlisted header was copied")
	}
}
