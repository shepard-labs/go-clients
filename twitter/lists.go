package twitter

import (
	"context"
	"fmt"
	"net/http"
)

// ── Lists CRUD ───────────────────────────────────────────────────────────────

// CreateList creates a List and returns it. The response data is a sparse List
// (only id/name populated). POST /2/lists, body {name, description?, private?}.
func (c *Client) CreateList(ctx context.Context, req *CreateListRequest) (*List, error) {
	if req == nil || req.Name == "" {
		return nil, fmt.Errorf("%w: list name required", ErrInvalidRequest)
	}

	var env dataEnvelope[List]
	if err := c.doJSON(ctx, http.MethodPost, "/2/lists", req, &env); err != nil {
		return nil, err
	}
	return dataOrErr(&env)
}

// GetList fetches a single List by id. GET /2/lists/{id}.
func (c *Client) GetList(ctx context.Context, id string, opts ...FieldOpt) (*List, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: id required", ErrInvalidRequest)
	}

	var env dataEnvelope[List]
	if err := c.doJSON(ctx, http.MethodGet, "/2/lists/"+id+applyFieldOpts(opts), nil, &env); err != nil {
		return nil, err
	}
	return dataOrErr(&env)
}

// UpdateList updates a List's metadata and reports whether it was updated. No
// field is server-required; a nil request is rejected client-side, but an empty
// (non-nil) request is valid and sent as {}. PUT /2/lists/{id}.
func (c *Client) UpdateList(ctx context.Context, id string, req *UpdateListRequest) (bool, error) {
	if id == "" {
		return false, fmt.Errorf("%w: id required", ErrInvalidRequest)
	}
	if req == nil {
		return false, fmt.Errorf("%w: update request required", ErrInvalidRequest)
	}

	var env dataEnvelope[updatedResult]
	if err := c.doJSON(ctx, http.MethodPut, "/2/lists/"+id, req, &env); err != nil {
		return false, err
	}
	return env.Data.Updated, nil
}

// DeleteList deletes a List by id and reports whether it was deleted.
// DELETE /2/lists/{id}.
func (c *Client) DeleteList(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, fmt.Errorf("%w: id required", ErrInvalidRequest)
	}

	var env dataEnvelope[deleteResult]
	if err := c.doJSON(ctx, http.MethodDelete, "/2/lists/"+id, nil, &env); err != nil {
		return false, err
	}
	return env.Data.Deleted, nil
}

// AddListMember adds a user to a List and reports whether they are now a member.
// POST /2/lists/{id}/members, body {user_id}.
func (c *Client) AddListMember(ctx context.Context, listID, userID string) (bool, error) {
	if listID == "" || userID == "" {
		return false, fmt.Errorf("%w: listID and userID required", ErrInvalidRequest)
	}

	var env dataEnvelope[memberResult]
	if err := c.doJSON(ctx, http.MethodPost, "/2/lists/"+listID+"/members",
		userIDBody{UserID: userID}, &env); err != nil {
		return false, err
	}
	return env.Data.IsMember, nil
}

// RemoveListMember removes a user from a List and reports whether they are still
// a member (false on success). Both ids are path segments.
// DELETE /2/lists/{id}/members/{user_id}.
func (c *Client) RemoveListMember(ctx context.Context, listID, userID string) (bool, error) {
	if listID == "" || userID == "" {
		return false, fmt.Errorf("%w: listID and userID required", ErrInvalidRequest)
	}

	var env dataEnvelope[memberResult]
	if err := c.doJSON(ctx, http.MethodDelete,
		"/2/lists/"+listID+"/members/"+userID, nil, &env); err != nil {
		return false, err
	}
	return env.Data.IsMember, nil
}

// GetListMembers fetches the members of a List. The request cursor is
// pagination_token; feed UserPage.Meta.NextToken back into
// PageParams.PaginationToken to page forward. GET /2/lists/{id}/members.
func (c *Client) GetListMembers(ctx context.Context, listID string, p *PageParams) (*UserPage, error) {
	if listID == "" {
		return nil, fmt.Errorf("%w: listID required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]User]
	if err := c.doJSON(ctx, http.MethodGet, "/2/lists/"+listID+"/members"+pageQuery(p), nil, &env); err != nil {
		return nil, err
	}
	return &UserPage{Users: env.Data, Meta: env.Meta}, nil
}

// GetListFollowers fetches the followers of a List. The request cursor is
// pagination_token. GET /2/lists/{id}/followers.
func (c *Client) GetListFollowers(ctx context.Context, listID string, p *PageParams) (*UserPage, error) {
	if listID == "" {
		return nil, fmt.Errorf("%w: listID required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]User]
	if err := c.doJSON(ctx, http.MethodGet, "/2/lists/"+listID+"/followers"+pageQuery(p), nil, &env); err != nil {
		return nil, err
	}
	return &UserPage{Users: env.Data, Meta: env.Meta}, nil
}

// GetListTweets fetches a List's tweet timeline. The request cursor is
// pagination_token; feed TweetPage.Meta.NextToken back into
// TimelineParams.PaginationToken to page forward. GET /2/lists/{id}/tweets.
func (c *Client) GetListTweets(ctx context.Context, listID string, p *TimelineParams) (*TweetPage, error) {
	if listID == "" {
		return nil, fmt.Errorf("%w: listID required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]Tweet]
	if err := c.doJSON(ctx, http.MethodGet, "/2/lists/"+listID+"/tweets"+timelineQuery(p), nil, &env); err != nil {
		return nil, err
	}
	return &TweetPage{Tweets: env.Data, Meta: env.Meta}, nil
}

// ── List membership (user side) ──────────────────────────────────────────────

// GetOwnedLists fetches the Lists a user owns. The request cursor is
// pagination_token. GET /2/users/{id}/owned_lists.
func (c *Client) GetOwnedLists(ctx context.Context, userID string, p *PageParams) (*ListPage, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: userID required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]List]
	if err := c.doJSON(ctx, http.MethodGet, "/2/users/"+userID+"/owned_lists"+pageQuery(p), nil, &env); err != nil {
		return nil, err
	}
	return &ListPage{Lists: env.Data, Meta: env.Meta}, nil
}

// GetListMemberships fetches the Lists a user is a member of. The request cursor
// is pagination_token. GET /2/users/{id}/list_memberships.
func (c *Client) GetListMemberships(ctx context.Context, userID string, p *PageParams) (*ListPage, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: userID required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]List]
	if err := c.doJSON(ctx, http.MethodGet, "/2/users/"+userID+"/list_memberships"+pageQuery(p), nil, &env); err != nil {
		return nil, err
	}
	return &ListPage{Lists: env.Data, Meta: env.Meta}, nil
}

// GetFollowedLists fetches the Lists a user follows. The request cursor is
// pagination_token. GET /2/users/{id}/followed_lists.
func (c *Client) GetFollowedLists(ctx context.Context, userID string, p *PageParams) (*ListPage, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: userID required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]List]
	if err := c.doJSON(ctx, http.MethodGet, "/2/users/"+userID+"/followed_lists"+pageQuery(p), nil, &env); err != nil {
		return nil, err
	}
	return &ListPage{Lists: env.Data, Meta: env.Meta}, nil
}

// FollowList makes userID follow a List and reports whether they now follow it.
// POST /2/users/{id}/followed_lists, body {list_id}.
func (c *Client) FollowList(ctx context.Context, userID, listID string) (bool, error) {
	if userID == "" || listID == "" {
		return false, fmt.Errorf("%w: userID and listID required", ErrInvalidRequest)
	}

	var env dataEnvelope[followResult]
	if err := c.doJSON(ctx, http.MethodPost, "/2/users/"+userID+"/followed_lists",
		listIDBody{ListID: listID}, &env); err != nil {
		return false, err
	}
	return env.Data.Following, nil
}

// UnfollowList makes userID stop following a List and reports whether they still
// follow it (false on success). Both ids are path segments.
// DELETE /2/users/{id}/followed_lists/{list_id}.
func (c *Client) UnfollowList(ctx context.Context, userID, listID string) (bool, error) {
	if userID == "" || listID == "" {
		return false, fmt.Errorf("%w: userID and listID required", ErrInvalidRequest)
	}

	var env dataEnvelope[followResult]
	if err := c.doJSON(ctx, http.MethodDelete,
		"/2/users/"+userID+"/followed_lists/"+listID, nil, &env); err != nil {
		return false, err
	}
	return env.Data.Following, nil
}

// GetPinnedLists fetches the Lists a user has pinned. This endpoint has no
// pagination, so it returns a plain slice (no Meta). GET /2/users/{id}/pinned_lists.
func (c *Client) GetPinnedLists(ctx context.Context, userID string, opts ...FieldOpt) ([]List, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: userID required", ErrInvalidRequest)
	}

	var env dataEnvelope[[]List]
	if err := c.doJSON(ctx, http.MethodGet, "/2/users/"+userID+"/pinned_lists"+applyFieldOpts(opts), nil, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// PinList pins a List for userID and reports whether it is now pinned.
// POST /2/users/{id}/pinned_lists, body {list_id}.
func (c *Client) PinList(ctx context.Context, userID, listID string) (bool, error) {
	if userID == "" || listID == "" {
		return false, fmt.Errorf("%w: userID and listID required", ErrInvalidRequest)
	}

	var env dataEnvelope[pinnedResult]
	if err := c.doJSON(ctx, http.MethodPost, "/2/users/"+userID+"/pinned_lists",
		listIDBody{ListID: listID}, &env); err != nil {
		return false, err
	}
	return env.Data.Pinned, nil
}

// UnpinList unpins a List for userID and reports whether it is still pinned
// (false on success). Both ids are path segments.
// DELETE /2/users/{id}/pinned_lists/{list_id}.
func (c *Client) UnpinList(ctx context.Context, userID, listID string) (bool, error) {
	if userID == "" || listID == "" {
		return false, fmt.Errorf("%w: userID and listID required", ErrInvalidRequest)
	}

	var env dataEnvelope[pinnedResult]
	if err := c.doJSON(ctx, http.MethodDelete,
		"/2/users/"+userID+"/pinned_lists/"+listID, nil, &env); err != nil {
		return false, err
	}
	return env.Data.Pinned, nil
}
