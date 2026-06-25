package twitter

import (
	"context"
	"fmt"
	"net/http"
)

// Like likes a tweet on behalf of userID and reports whether it is now liked.
// POST /2/users/{id}/likes, body {tweet_id}.
func (c *Client) Like(ctx context.Context, userID, tweetID string) (bool, error) {
	if userID == "" || tweetID == "" {
		return false, fmt.Errorf("%w: userID and tweetID required", ErrInvalidRequest)
	}

	var env dataEnvelope[likeResult]
	if err := c.doJSON(ctx, http.MethodPost, "/2/users/"+userID+"/likes",
		tweetIDBody{TweetID: tweetID}, &env); err != nil {
		return false, err
	}
	return env.Data.Liked, nil
}

// Unlike removes userID's like from a tweet and reports whether it is still
// liked (false on success). DELETE /2/users/{id}/likes/{tweet_id}.
func (c *Client) Unlike(ctx context.Context, userID, tweetID string) (bool, error) {
	if userID == "" || tweetID == "" {
		return false, fmt.Errorf("%w: userID and tweetID required", ErrInvalidRequest)
	}

	var env dataEnvelope[likeResult]
	if err := c.doJSON(ctx, http.MethodDelete, "/2/users/"+userID+"/likes/"+tweetID, nil, &env); err != nil {
		return false, err
	}
	return env.Data.Liked, nil
}

// Retweet reposts a tweet on behalf of userID and reports whether it is now
// retweeted. POST /2/users/{id}/retweets, body {tweet_id}.
func (c *Client) Retweet(ctx context.Context, userID, tweetID string) (bool, error) {
	if userID == "" || tweetID == "" {
		return false, fmt.Errorf("%w: userID and tweetID required", ErrInvalidRequest)
	}

	var env dataEnvelope[retweetResult]
	if err := c.doJSON(ctx, http.MethodPost, "/2/users/"+userID+"/retweets",
		tweetIDBody{TweetID: tweetID}, &env); err != nil {
		return false, err
	}
	return env.Data.Retweeted, nil
}

// Unretweet removes userID's repost of a tweet and reports whether it is still
// retweeted (false on success). The source tweet id is the path segment.
// DELETE /2/users/{id}/retweets/{source_tweet_id}.
func (c *Client) Unretweet(ctx context.Context, userID, tweetID string) (bool, error) {
	if userID == "" || tweetID == "" {
		return false, fmt.Errorf("%w: userID and tweetID required", ErrInvalidRequest)
	}

	var env dataEnvelope[retweetResult]
	if err := c.doJSON(ctx, http.MethodDelete, "/2/users/"+userID+"/retweets/"+tweetID, nil, &env); err != nil {
		return false, err
	}
	return env.Data.Retweeted, nil
}

// HideReply hides or unhides a reply to one of the authenticated user's tweets
// and reports the resulting hidden state. PUT /2/tweets/{tweet_id}/hidden,
// body {hidden}.
func (c *Client) HideReply(ctx context.Context, tweetID string, hidden bool) (bool, error) {
	if tweetID == "" {
		return false, fmt.Errorf("%w: tweetID required", ErrInvalidRequest)
	}

	var env dataEnvelope[hideResult]
	if err := c.doJSON(ctx, http.MethodPut, "/2/tweets/"+tweetID+"/hidden",
		hiddenBody{Hidden: hidden}, &env); err != nil {
		return false, err
	}
	return env.Data.Hidden, nil
}
