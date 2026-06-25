package twitter

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// GetTweetAnalytics fetches time-bucketed analytics for the given tweet ids.
// All of ids, start_time, end_time, and granularity are required.
// GET /2/tweets/analytics.
//
// Analytics requires an elevated X API access tier; without it the call returns
// HTTP 403 (APIError.IsForbidden).
func (c *Client) GetTweetAnalytics(ctx context.Context, p *AnalyticsParams) ([]TweetAnalytics, error) {
	if p == nil || len(p.IDs) == 0 || p.StartTime == "" || p.EndTime == "" || p.Granularity == "" {
		return nil, fmt.Errorf("%w: ids, start_time, end_time, granularity all required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]TweetAnalytics]
	if err := c.doJSON(ctx, http.MethodGet, "/2/tweets/analytics"+analyticsQuery(p), nil, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// GetMediaAnalytics fetches time-bucketed analytics for the given media keys.
// All of media_keys, start_time, end_time, and granularity are required.
// GET /2/media/analytics.
//
// Analytics requires an elevated X API access tier; without it the call returns
// HTTP 403 (APIError.IsForbidden).
func (c *Client) GetMediaAnalytics(ctx context.Context, p *MediaAnalyticsParams) ([]MediaAnalytics, error) {
	if p == nil || len(p.MediaKeys) == 0 || p.StartTime == "" || p.EndTime == "" || p.Granularity == "" {
		return nil, fmt.Errorf("%w: media_keys, start_time, end_time, granularity all required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]MediaAnalytics]
	if err := c.doJSON(ctx, http.MethodGet, "/2/media/analytics"+mediaAnalyticsQuery(p), nil, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// analyticsQuery builds the query string for GetTweetAnalytics. It assumes a
// non-nil, pre-validated p (the method validates p first). The returned string
// always has a leading "?".
func analyticsQuery(p *AnalyticsParams) string {
	v := url.Values{}
	v.Set("ids", strings.Join(p.IDs, ","))
	v.Set("start_time", p.StartTime)
	v.Set("end_time", p.EndTime)
	v.Set("granularity", p.Granularity)
	return "?" + v.Encode()
}

// mediaAnalyticsQuery builds the query string for GetMediaAnalytics. It assumes
// a non-nil, pre-validated p. The returned string always has a leading "?".
func mediaAnalyticsQuery(p *MediaAnalyticsParams) string {
	v := url.Values{}
	v.Set("media_keys", strings.Join(p.MediaKeys, ","))
	v.Set("start_time", p.StartTime)
	v.Set("end_time", p.EndTime)
	v.Set("granularity", p.Granularity)
	return "?" + v.Encode()
}
