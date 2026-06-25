package twitter

import (
	"context"
	"net/http"
)

// GetPersonalizedTrends fetches the authenticated user's personalized trends.
// This endpoint takes no parameters and has no pagination.
// GET /2/users/personalized_trends.
func (c *Client) GetPersonalizedTrends(ctx context.Context) ([]Trend, error) {
	var env dataEnvelope[[]Trend]
	if err := c.doJSON(ctx, http.MethodGet, "/2/users/personalized_trends", nil, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}
