package twitter

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// SearchRecent searches the last 7 days of public tweets. query is required.
// GET /2/tweets/search/recent. NOTE: the request cursor is next_token (not
// pagination_token); feed TweetPage.Meta.NextToken back into
// SearchParams.NextToken to page forward.
func (c *Client) SearchRecent(ctx context.Context, query string, p *SearchParams) (*TweetPage, error) {
	if query == "" {
		return nil, fmt.Errorf("%w: query required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]Tweet]
	if err := c.doJSON(ctx, http.MethodGet, "/2/tweets/search/recent"+searchQuery(query, p), nil, &env); err != nil {
		return nil, err
	}
	return &TweetPage{Tweets: env.Data, Meta: env.Meta}, nil
}

// GetMentions fetches tweets mentioning a user. The request cursor is
// pagination_token. GET /2/users/{id}/mentions.
func (c *Client) GetMentions(ctx context.Context, userID string, p *TimelineParams) (*TweetPage, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: userID required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]Tweet]
	if err := c.doJSON(ctx, http.MethodGet, "/2/users/"+userID+"/mentions"+timelineQuery(p), nil, &env); err != nil {
		return nil, err
	}
	return &TweetPage{Tweets: env.Data, Meta: env.Meta}, nil
}

// GetHomeTimeline fetches a user's reverse-chronological home timeline. The
// request cursor is pagination_token.
// GET /2/users/{id}/timelines/reverse_chronological.
func (c *Client) GetHomeTimeline(ctx context.Context, userID string, p *TimelineParams) (*TweetPage, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: userID required", ErrInvalidRequest)
	}

	path := "/2/users/" + userID + "/timelines/reverse_chronological" + timelineQuery(p)
	var env dataEnvelope[[]Tweet]
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &env); err != nil {
		return nil, err
	}
	return &TweetPage{Tweets: env.Data, Meta: env.Meta}, nil
}

// GetLikingUsers fetches the users who liked a tweet. The request cursor is
// pagination_token. GET /2/tweets/{id}/liking_users.
func (c *Client) GetLikingUsers(ctx context.Context, tweetID string, p *PageParams) (*UserPage, error) {
	if tweetID == "" {
		return nil, fmt.Errorf("%w: tweetID required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]User]
	if err := c.doJSON(ctx, http.MethodGet, "/2/tweets/"+tweetID+"/liking_users"+pageQuery(p), nil, &env); err != nil {
		return nil, err
	}
	return &UserPage{Users: env.Data, Meta: env.Meta}, nil
}

// GetRetweetedBy fetches the users who reposted a tweet. The request cursor is
// pagination_token. GET /2/tweets/{id}/retweeted_by.
func (c *Client) GetRetweetedBy(ctx context.Context, tweetID string, p *PageParams) (*UserPage, error) {
	if tweetID == "" {
		return nil, fmt.Errorf("%w: tweetID required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]User]
	if err := c.doJSON(ctx, http.MethodGet, "/2/tweets/"+tweetID+"/retweeted_by"+pageQuery(p), nil, &env); err != nil {
		return nil, err
	}
	return &UserPage{Users: env.Data, Meta: env.Meta}, nil
}

// GetQuoteTweets fetches the tweets quoting a tweet. The request cursor is
// pagination_token. GET /2/tweets/{id}/quote_tweets.
func (c *Client) GetQuoteTweets(ctx context.Context, tweetID string, p *TimelineParams) (*TweetPage, error) {
	if tweetID == "" {
		return nil, fmt.Errorf("%w: tweetID required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]Tweet]
	if err := c.doJSON(ctx, http.MethodGet, "/2/tweets/"+tweetID+"/quote_tweets"+timelineQuery(p), nil, &env); err != nil {
		return nil, err
	}
	return &TweetPage{Tweets: env.Data, Meta: env.Meta}, nil
}

// pageQuery builds the query string for follower/member-style reads from
// PageParams, including any embedded field options. The cursor key is
// pagination_token. The returned string has a leading "?" when non-empty.
func pageQuery(p *PageParams) string {
	if p == nil {
		return ""
	}

	v := url.Values{}
	if p.MaxResults > 0 {
		v.Set("max_results", strconv.Itoa(p.MaxResults))
	}
	if p.PaginationToken != "" {
		v.Set("pagination_token", p.PaginationToken)
	}
	for _, opt := range p.Fields {
		opt(v)
	}

	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// searchQuery builds the query string for SearchRecent. query is required and
// is always emitted, so this never early-returns on a nil p. The cursor key is
// next_token (NOT pagination_token). The returned string always has a leading
// "?" (query is non-empty).
func searchQuery(query string, p *SearchParams) string {
	v := url.Values{}
	v.Set("query", query)
	if p != nil {
		if p.MaxResults > 0 {
			v.Set("max_results", strconv.Itoa(p.MaxResults))
		}
		if p.NextToken != "" {
			v.Set("next_token", p.NextToken)
		}
		if p.StartTime != "" {
			v.Set("start_time", p.StartTime)
		}
		if p.EndTime != "" {
			v.Set("end_time", p.EndTime)
		}
		if p.SinceID != "" {
			v.Set("since_id", p.SinceID)
		}
		if p.UntilID != "" {
			v.Set("until_id", p.UntilID)
		}
		if p.SortOrder != "" {
			v.Set("sort_order", p.SortOrder)
		}
		for _, opt := range p.Fields {
			opt(v)
		}
	}
	return "?" + v.Encode()
}
