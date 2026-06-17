package main

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// bearerAuth returns middleware that requires an "Authorization: Bearer <key>"
// header matching apiKey. The comparison is constant-time to avoid leaking the
// key through timing. On failure it aborts with 401 and a generic message.
func bearerAuth(apiKey string) gin.HandlerFunc {
	want := []byte(apiKey)
	return func(c *gin.Context) {
		const prefix = "Bearer "
		h := c.GetHeader("Authorization")
		if !strings.HasPrefix(h, prefix) {
			abortError(c, http.StatusUnauthorized, "unauthorized")
			return
		}
		got := []byte(strings.TrimPrefix(h, prefix))
		// subtle.ConstantTimeCompare returns 0 when lengths differ; that is the
		// desired "not equal" outcome.
		if subtle.ConstantTimeCompare(got, want) != 1 {
			abortError(c, http.StatusUnauthorized, "unauthorized")
			return
		}
		c.Next()
	}
}

// limitBody caps the request body size. Reads beyond max return an error that
// handlers surface as 400, preventing unbounded memory use from large bodies.
func limitBody(max int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, max)
		c.Next()
	}
}

// withTimeout attaches a per-request deadline to the request context so a slow
// upstream cannot tie up a handler indefinitely.
func withTimeout(d time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
