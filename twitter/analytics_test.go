package twitter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGetTweetAnalyticsParsesTimestampedMetrics(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		writeJSON(w, http.StatusOK, dataEnvelope[[]TweetAnalytics]{
			Data: []TweetAnalytics{{
				ID: "1455",
				TimestampedMetrics: []TimestampedMetric{{
					Timestamp: "2026-01-01T00:00:00Z",
					Metrics:   map[string]int64{"impressions": 1200, "likes": 34, "engagements": 56},
				}},
			}},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	rows, err := c.GetTweetAnalytics(context.Background(), &AnalyticsParams{
		IDs:         []string{"1455", "1456"},
		StartTime:   "2026-01-01T00:00:00Z",
		EndTime:     "2026-01-08T00:00:00Z",
		Granularity: "weekly", // weekly is valid for tweet analytics
	})
	if err != nil {
		t.Fatalf("GetTweetAnalytics: %v", err)
	}
	if gotPath != "/2/tweets/analytics" {
		t.Fatalf("path = %q", gotPath)
	}
	if len(rows) != 1 || rows[0].ID != "1455" || len(rows[0].TimestampedMetrics) != 1 {
		t.Fatalf("unexpected rows: %#v", rows)
	}
	if got := rows[0].TimestampedMetrics[0].Metrics["impressions"]; got != 1200 {
		t.Fatalf("impressions = %d, want 1200", got)
	}
	if !strings.Contains(gotQuery, "ids=1455%2C1456") {
		t.Fatalf("expected comma-joined ids, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "granularity=weekly") {
		t.Fatalf("expected granularity=weekly, got %q", gotQuery)
	}
}

func TestGetTweetAnalyticsValidatesRequiredParams(t *testing.T) {
	var called int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	cases := []*AnalyticsParams{
		nil,
		{StartTime: "s", EndTime: "e", Granularity: "daily"},       // no ids
		{IDs: []string{"1"}, EndTime: "e", Granularity: "daily"},   // no start
		{IDs: []string{"1"}, StartTime: "s", Granularity: "daily"}, // no end
		{IDs: []string{"1"}, StartTime: "s", EndTime: "e"},         // no granularity
	}
	for i, p := range cases {
		if _, err := c.GetTweetAnalytics(context.Background(), p); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("case %d: expected ErrInvalidRequest, got %v", i, err)
		}
	}
	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("HTTP endpoint called despite invalid params")
	}
}

func TestGetMediaAnalyticsParsesAndJoinsKeys(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		writeJSON(w, http.StatusOK, dataEnvelope[[]MediaAnalytics]{
			Data: []MediaAnalytics{{
				MediaKey: "13_123",
				TimestampedMetrics: []TimestampedMetric{{
					Timestamp: "2026-01-01T00:00:00Z",
					Metrics:   map[string]int64{"video_views": 999, "playback_complete": 120},
				}},
			}},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	rows, err := c.GetMediaAnalytics(context.Background(), &MediaAnalyticsParams{
		MediaKeys:   []string{"13_123", "13_124"},
		StartTime:   "2026-01-01T00:00:00Z",
		EndTime:     "2026-01-02T00:00:00Z",
		Granularity: "daily", // media analytics: never weekly
	})
	if err != nil {
		t.Fatalf("GetMediaAnalytics: %v", err)
	}
	if gotPath != "/2/media/analytics" {
		t.Fatalf("path = %q", gotPath)
	}
	if len(rows) != 1 || rows[0].MediaKey != "13_123" {
		t.Fatalf("unexpected rows: %#v", rows)
	}
	if got := rows[0].TimestampedMetrics[0].Metrics["video_views"]; got != 999 {
		t.Fatalf("video_views = %d, want 999", got)
	}
	if !strings.Contains(gotQuery, "media_keys=13_123%2C13_124") {
		t.Fatalf("expected comma-joined media_keys, got %q", gotQuery)
	}
}

func TestGetMediaAnalyticsValidatesRequiredParams(t *testing.T) {
	c := newTestClient("http://twitter.test")
	if _, err := c.GetMediaAnalytics(context.Background(), nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil params: %v", err)
	}
	if _, err := c.GetMediaAnalytics(context.Background(), &MediaAnalyticsParams{
		StartTime: "s", EndTime: "e", Granularity: "daily",
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing media_keys: %v", err)
	}
}

// Analytics needs an elevated access tier; without it the API returns 403.
func TestGetTweetAnalyticsForbiddenSurfacesAsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, problemBody{
			Title:  "Forbidden",
			Detail: "Your account is not permitted to access this endpoint.",
			Type:   "about:blank",
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.GetTweetAnalytics(context.Background(), &AnalyticsParams{
		IDs:         []string{"1455"},
		StartTime:   "2026-01-01T00:00:00Z",
		EndTime:     "2026-01-08T00:00:00Z",
		Granularity: "total",
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T %v", err, err)
	}
	if !apiErr.IsForbidden() {
		t.Fatalf("expected IsForbidden, got %#v", apiErr)
	}
}
