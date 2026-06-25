package twitter

import (
	"context"
	"fmt"
	"net/http"
)

// GetBookmarks fetches a user's bookmarked tweets. The request cursor is
// pagination_token; feed TweetPage.Meta.NextToken back into
// TimelineParams.PaginationToken to page forward. GET /2/users/{id}/bookmarks.
func (c *Client) GetBookmarks(ctx context.Context, userID string, p *TimelineParams) (*TweetPage, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: userID required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]Tweet]
	if err := c.doJSON(ctx, http.MethodGet, "/2/users/"+userID+"/bookmarks"+timelineQuery(p), nil, &env); err != nil {
		return nil, err
	}
	return &TweetPage{Tweets: env.Data, Meta: env.Meta}, nil
}

// AddBookmark bookmarks a tweet for userID and reports whether it is now
// bookmarked. POST /2/users/{id}/bookmarks, body {tweet_id}.
func (c *Client) AddBookmark(ctx context.Context, userID, tweetID string) (bool, error) {
	if userID == "" || tweetID == "" {
		return false, fmt.Errorf("%w: userID and tweetID required", ErrInvalidRequest)
	}

	var env dataEnvelope[bookmarkResult]
	if err := c.doJSON(ctx, http.MethodPost, "/2/users/"+userID+"/bookmarks",
		tweetIDBody{TweetID: tweetID}, &env); err != nil {
		return false, err
	}
	return env.Data.Bookmarked, nil
}

// RemoveBookmark removes a tweet from userID's bookmarks and reports whether it
// is still bookmarked (false on success). Both ids are path segments.
// DELETE /2/users/{id}/bookmarks/{tweet_id}.
func (c *Client) RemoveBookmark(ctx context.Context, userID, tweetID string) (bool, error) {
	if userID == "" || tweetID == "" {
		return false, fmt.Errorf("%w: userID and tweetID required", ErrInvalidRequest)
	}

	var env dataEnvelope[bookmarkResult]
	if err := c.doJSON(ctx, http.MethodDelete,
		"/2/users/"+userID+"/bookmarks/"+tweetID, nil, &env); err != nil {
		return false, err
	}
	return env.Data.Bookmarked, nil
}
