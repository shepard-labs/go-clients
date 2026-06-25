package twitter

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
)

// dataOrErr applies the missing-data rule after a single-object 2xx unmarshal.
// X can return a 2xx whose body carries only errors[] and an absent/null data;
// returning &env.Data then silently yields a zero-value struct. When data is
// the zero value and errors[] is non-empty, return an *APIError instead.
func dataOrErr[T any](env *dataEnvelope[T]) (*T, error) {
	if reflect.ValueOf(env.Data).IsZero() && len(env.Errors) > 0 {
		return nil, &APIError{
			StatusCode: http.StatusOK,
			Title:      env.Errors[0].Title,
			Detail:     env.Errors[0].Detail,
			Type:       env.Errors[0].Type,
			Problems:   env.Errors,
		}
	}
	return &env.Data, nil
}

// CreateTweet publishes a tweet. POST /2/tweets.
//
// Only a fully-empty request is rejected client-side; reply-only, quote-only,
// poll-only, and media-only posts are all valid.
func (c *Client) CreateTweet(ctx context.Context, req *CreateTweetRequest) (*Tweet, error) {
	if req == nil || (req.Text == "" && req.Media == nil && req.Poll == nil &&
		req.QuoteTweetID == "" && req.Reply == nil) {
		return nil, fmt.Errorf("%w: text, media, poll, quote_tweet_id, or reply required", ErrInvalidRequest)
	}

	var env dataEnvelope[Tweet]
	if err := c.doJSON(ctx, http.MethodPost, "/2/tweets", req, &env); err != nil {
		return nil, err
	}
	return dataOrErr(&env)
}

// DeleteTweet deletes a tweet by id and reports whether it was deleted.
// DELETE /2/tweets/{id}.
func (c *Client) DeleteTweet(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, fmt.Errorf("%w: id required", ErrInvalidRequest)
	}

	var env dataEnvelope[deleteResult]
	if err := c.doJSON(ctx, http.MethodDelete, "/2/tweets/"+id, nil, &env); err != nil {
		return false, err
	}
	return env.Data.Deleted, nil
}

// GetTweet fetches a single tweet by id. GET /2/tweets/{id}.
func (c *Client) GetTweet(ctx context.Context, id string, opts ...FieldOpt) (*Tweet, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: id required", ErrInvalidRequest)
	}

	var env dataEnvelope[Tweet]
	if err := c.doJSON(ctx, http.MethodGet, "/2/tweets/"+id+applyFieldOpts(opts), nil, &env); err != nil {
		return nil, err
	}
	return dataOrErr(&env)
}

// GetTweets fetches multiple tweets by id. GET /2/tweets?ids=.
func (c *Client) GetTweets(ctx context.Context, ids []string, opts ...FieldOpt) ([]Tweet, error) {
	joined, err := joinIDs(ids)
	if err != nil {
		return nil, err
	}

	opts = append([]FieldOpt{withIDs(joined)}, opts...)
	var env dataEnvelope[[]Tweet]
	if err := c.doJSON(ctx, http.MethodGet, "/2/tweets"+applyFieldOpts(opts), nil, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}
