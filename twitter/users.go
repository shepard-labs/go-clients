package twitter

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Me fetches the authenticated user's profile. GET /2/users/me.
func (c *Client) Me(ctx context.Context, opts ...FieldOpt) (*User, error) {
	var env dataEnvelope[User]
	if err := c.doJSON(ctx, http.MethodGet, "/2/users/me"+applyFieldOpts(opts), nil, &env); err != nil {
		return nil, err
	}
	return dataOrErr(&env)
}

// GetUser fetches a user by id. GET /2/users/{id}.
func (c *Client) GetUser(ctx context.Context, id string, opts ...FieldOpt) (*User, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: id required", ErrInvalidRequest)
	}

	var env dataEnvelope[User]
	if err := c.doJSON(ctx, http.MethodGet, "/2/users/"+id+applyFieldOpts(opts), nil, &env); err != nil {
		return nil, err
	}
	return dataOrErr(&env)
}

// GetUserByUsername fetches a user by username. GET /2/users/by/username/{username}.
func (c *Client) GetUserByUsername(ctx context.Context, username string, opts ...FieldOpt) (*User, error) {
	if username == "" {
		return nil, fmt.Errorf("%w: username required", ErrInvalidRequest)
	}

	var env dataEnvelope[User]
	if err := c.doJSON(ctx, http.MethodGet, "/2/users/by/username/"+username+applyFieldOpts(opts), nil, &env); err != nil {
		return nil, err
	}
	return dataOrErr(&env)
}

// GetUsers fetches multiple users by id. GET /2/users?ids=.
func (c *Client) GetUsers(ctx context.Context, ids []string, opts ...FieldOpt) ([]User, error) {
	joined, err := joinIDs(ids)
	if err != nil {
		return nil, err
	}

	opts = append([]FieldOpt{withIDs(joined)}, opts...)
	var env dataEnvelope[[]User]
	if err := c.doJSON(ctx, http.MethodGet, "/2/users"+applyFieldOpts(opts), nil, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// GetUserTweets fetches a user's timeline. GET /2/users/{id}/tweets. The request
// cursor is pagination_token; feed TweetPage.Meta.NextToken back into
// TimelineParams.PaginationToken to page forward.
func (c *Client) GetUserTweets(ctx context.Context, id string, p *TimelineParams) (*TweetPage, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: id required", ErrInvalidRequest)
	}

	path := "/2/users/" + id + "/tweets" + timelineQuery(p)
	var env dataEnvelope[[]Tweet]
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &env); err != nil {
		return nil, err
	}
	return &TweetPage{Tweets: env.Data, Meta: env.Meta}, nil
}

// timelineQuery builds the query string for GetUserTweets, including any
// embedded field options. The returned string has a leading "?" when non-empty.
func timelineQuery(p *TimelineParams) string {
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
	if len(p.Exclude) > 0 {
		v.Set("exclude", strings.Join(p.Exclude, ","))
	}
	if p.SinceID != "" {
		v.Set("since_id", p.SinceID)
	}
	if p.UntilID != "" {
		v.Set("until_id", p.UntilID)
	}
	if p.StartTime != "" {
		v.Set("start_time", p.StartTime)
	}
	if p.EndTime != "" {
		v.Set("end_time", p.EndTime)
	}
	for _, opt := range p.Fields {
		opt(v)
	}

	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// ── Tier 2: followers / following ───────────────────────────────────────────

// GetFollowers fetches a user's followers. The request cursor is
// pagination_token; feed UserPage.Meta.NextToken back into
// PageParams.PaginationToken to page forward. GET /2/users/{id}/followers.
func (c *Client) GetFollowers(ctx context.Context, userID string, p *PageParams) (*UserPage, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: userID required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]User]
	if err := c.doJSON(ctx, http.MethodGet, "/2/users/"+userID+"/followers"+pageQuery(p), nil, &env); err != nil {
		return nil, err
	}
	return &UserPage{Users: env.Data, Meta: env.Meta}, nil
}

// GetFollowing fetches the users a user follows. The request cursor is
// pagination_token. GET /2/users/{id}/following.
func (c *Client) GetFollowing(ctx context.Context, userID string, p *PageParams) (*UserPage, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: userID required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]User]
	if err := c.doJSON(ctx, http.MethodGet, "/2/users/"+userID+"/following"+pageQuery(p), nil, &env); err != nil {
		return nil, err
	}
	return &UserPage{Users: env.Data, Meta: env.Meta}, nil
}

// Follow makes sourceUserID follow targetUserID. A protected target yields
// PendingFollow=true until the request is accepted. POST /2/users/{id}/following,
// body {target_user_id}.
func (c *Client) Follow(ctx context.Context, sourceUserID, targetUserID string) (*FollowResult, error) {
	if sourceUserID == "" || targetUserID == "" {
		return nil, fmt.Errorf("%w: sourceUserID and targetUserID required", ErrInvalidRequest)
	}

	var env dataEnvelope[FollowResult]
	if err := c.doJSON(ctx, http.MethodPost, "/2/users/"+sourceUserID+"/following",
		targetIDBody{TargetUserID: targetUserID}, &env); err != nil {
		return nil, err
	}
	return dataOrErr(&env)
}

// Unfollow makes sourceUserID stop following targetUserID and reports whether
// it still follows (false on success). Both ids are path segments.
// DELETE /2/users/{source_user_id}/following/{target_user_id}.
func (c *Client) Unfollow(ctx context.Context, sourceUserID, targetUserID string) (bool, error) {
	if sourceUserID == "" || targetUserID == "" {
		return false, fmt.Errorf("%w: sourceUserID and targetUserID required", ErrInvalidRequest)
	}

	var env dataEnvelope[unfollowResult]
	if err := c.doJSON(ctx, http.MethodDelete,
		"/2/users/"+sourceUserID+"/following/"+targetUserID, nil, &env); err != nil {
		return false, err
	}
	return env.Data.Following, nil
}

// SearchUsers searches for users by query. query is required. The request
// cursor is next_token (NOT pagination_token); the /2/users/search meta has no
// result_count. GET /2/users/search.
func (c *Client) SearchUsers(ctx context.Context, query string, p *UserSearchParams) (*UserPage, error) {
	if query == "" {
		return nil, fmt.Errorf("%w: query required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]User]
	if err := c.doJSON(ctx, http.MethodGet, "/2/users/search"+userSearchQuery(query, p), nil, &env); err != nil {
		return nil, err
	}
	return &UserPage{Users: env.Data, Meta: env.Meta}, nil
}

// userSearchQuery builds the query string for SearchUsers. query is required
// and always emitted, so this never early-returns on a nil p. The cursor key is
// next_token (NOT pagination_token).
func userSearchQuery(query string, p *UserSearchParams) string {
	v := url.Values{}
	v.Set("query", query)
	if p != nil {
		if p.MaxResults > 0 {
			v.Set("max_results", strconv.Itoa(p.MaxResults))
		}
		if p.NextToken != "" {
			v.Set("next_token", p.NextToken)
		}
		for _, opt := range p.Fields {
			opt(v)
		}
	}
	return "?" + v.Encode()
}
