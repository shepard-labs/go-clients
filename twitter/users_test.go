package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGetFollowersPagesOnPaginationToken(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		writeJSON(w, http.StatusOK, dataEnvelope[[]User]{
			Data: []User{{ID: "1", Username: "a"}},
			Meta: &PageMeta{ResultCount: 1, NextToken: "FN"},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	page, err := c.GetFollowers(context.Background(), "42", &PageParams{PaginationToken: "CUR"})
	if err != nil {
		t.Fatalf("GetFollowers: %v", err)
	}
	if gotPath != "/2/users/42/followers" {
		t.Fatalf("path = %q", gotPath)
	}
	if page.Meta == nil || page.Meta.NextToken != "FN" {
		t.Fatalf("expected meta.next_token=FN, got %#v", page.Meta)
	}
	if !strings.Contains(gotQuery, "pagination_token=CUR") {
		t.Fatalf("expected pagination_token in request, got %q", gotQuery)
	}
	if strings.Contains(gotQuery, "next_token=") {
		t.Fatalf("request must not send next_token: %q", gotQuery)
	}
}

func TestGetFollowingPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(w, http.StatusOK, dataEnvelope[[]User]{Data: []User{{ID: "1"}}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.GetFollowing(context.Background(), "42", nil); err != nil {
		t.Fatalf("GetFollowing: %v", err)
	}
	if gotPath != "/2/users/42/following" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestFollowPostsTargetIDAndParsesResult(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody targetIDBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, http.StatusOK, dataEnvelope[FollowResult]{
			Data: FollowResult{Following: true, PendingFollow: false},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	res, err := c.Follow(context.Background(), "42", "99")
	if err != nil {
		t.Fatalf("Follow: %v", err)
	}
	if !res.Following || res.PendingFollow {
		t.Fatalf("unexpected result: %#v", res)
	}
	if gotMethod != http.MethodPost || gotPath != "/2/users/42/following" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotBody.TargetUserID != "99" {
		t.Fatalf("body target_user_id = %q", gotBody.TargetUserID)
	}
}

func TestFollowPendingFollow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, dataEnvelope[FollowResult]{
			Data: FollowResult{Following: false, PendingFollow: true},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	res, err := c.Follow(context.Background(), "42", "99")
	if err != nil {
		t.Fatalf("Follow: %v", err)
	}
	if res.Following || !res.PendingFollow {
		t.Fatalf("expected pending follow on protected target, got %#v", res)
	}
}

// Follow routes through dataOrErr: a 2xx body carrying only errors[] (zero-value
// data) must surface as an *APIError, not a zero-value FollowResult.
func TestFollowMissingDataReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, dataEnvelope[FollowResult]{
			Errors: []Problem{{Title: "Forbidden", Detail: "cannot follow", Type: "about:blank"}},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.Follow(context.Background(), "42", "99")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T %v", err, err)
	}
	if apiErr.Title != "Forbidden" || len(apiErr.Problems) != 1 {
		t.Fatalf("unexpected APIError: %#v", apiErr)
	}
}

func TestUnfollowBuildsTwoPathParamURLNoBody(t *testing.T) {
	var gotMethod, gotPath string
	var gotBodyLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBodyLen = r.ContentLength
		writeJSON(w, http.StatusOK, dataEnvelope[unfollowResult]{Data: unfollowResult{Following: false}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	ok, err := c.Unfollow(context.Background(), "42", "99")
	if err != nil {
		t.Fatalf("Unfollow: %v", err)
	}
	if ok {
		t.Fatal("expected following=false after unfollow")
	}
	if gotMethod != http.MethodDelete || gotPath != "/2/users/42/following/99" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotBodyLen > 0 {
		t.Fatalf("DELETE must send no body, got content-length %d", gotBodyLen)
	}
}

func TestFollowUnfollowValidateEmpty(t *testing.T) {
	c := newTestClient("http://twitter.test")
	if _, err := c.Follow(context.Background(), "", "99"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Follow empty source: %v", err)
	}
	if _, err := c.Follow(context.Background(), "42", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Follow empty target: %v", err)
	}
	if _, err := c.Unfollow(context.Background(), "", "99"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Unfollow empty source: %v", err)
	}
	if _, err := c.GetFollowers(context.Background(), "", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("GetFollowers empty id: %v", err)
	}
	if _, err := c.GetFollowing(context.Background(), "", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("GetFollowing empty id: %v", err)
	}
}

func TestSearchUsersUsesNextTokenAndNoResultCount(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		// /2/users/search meta has no result_count — only the cursor tokens.
		writeJSON(w, http.StatusOK, dataEnvelope[[]User]{
			Data: []User{{ID: "1", Username: "a"}},
			Meta: &PageMeta{NextToken: "UN", PreviousToken: "UP"},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	page, err := c.SearchUsers(context.Background(), "ada", &UserSearchParams{MaxResults: 10, NextToken: "CUR"})
	if err != nil {
		t.Fatalf("SearchUsers: %v", err)
	}
	if gotPath != "/2/users/search" {
		t.Fatalf("path = %q", gotPath)
	}
	if page.Meta == nil || page.Meta.NextToken != "UN" || page.Meta.PreviousToken != "UP" {
		t.Fatalf("expected next/previous token round-trip, got %#v", page.Meta)
	}
	if page.Meta.ResultCount != 0 {
		t.Fatalf("users/search meta has no result_count, got %d", page.Meta.ResultCount)
	}
	if !strings.Contains(gotQuery, "query=ada") {
		t.Fatalf("expected query=ada, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "next_token=CUR") {
		t.Fatalf("expected next_token=CUR, got %q", gotQuery)
	}
	if strings.Contains(gotQuery, "pagination_token=") {
		t.Fatalf("user search must not send pagination_token: %q", gotQuery)
	}
}

func TestSearchUsersRequiresQuery(t *testing.T) {
	var called int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.SearchUsers(context.Background(), "", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("HTTP endpoint called despite empty query")
	}
}
