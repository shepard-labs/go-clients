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

func TestSearchRecentRequiresQuery(t *testing.T) {
	var called int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.SearchRecent(context.Background(), "", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("HTTP endpoint called despite empty query")
	}
}

func TestSearchRecentUsesNextTokenCursor(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		writeJSON(w, http.StatusOK, dataEnvelope[[]Tweet]{
			Data: []Tweet{{ID: "1", Text: "a"}},
			Meta: &PageMeta{ResultCount: 1, NextToken: "NEXT99"},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	page, err := c.SearchRecent(context.Background(), "golang -is:retweet", &SearchParams{
		MaxResults: 50,
		NextToken:  "CURSOR0",
		SortOrder:  "recency",
		Fields:     []FieldOpt{WithTweetFields("created_at")},
	})
	if err != nil {
		t.Fatalf("SearchRecent: %v", err)
	}
	if gotPath != "/2/tweets/search/recent" {
		t.Fatalf("path = %q", gotPath)
	}
	if page.Meta == nil || page.Meta.NextToken != "NEXT99" {
		t.Fatalf("expected meta.next_token=NEXT99, got %#v", page.Meta)
	}
	// query is required and must be present.
	if !strings.Contains(gotQuery, "query=golang") {
		t.Fatalf("expected query= in request, got %q", gotQuery)
	}
	// Search uses next_token, NOT pagination_token.
	if !strings.Contains(gotQuery, "next_token=CURSOR0") {
		t.Fatalf("expected next_token=CURSOR0, got %q", gotQuery)
	}
	if strings.Contains(gotQuery, "pagination_token=") {
		t.Fatalf("search must not send pagination_token: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "sort_order=recency") {
		t.Fatalf("expected sort_order=recency, got %q", gotQuery)
	}
}

func TestSearchRecentNilParamsStillSendsQuery(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		writeJSON(w, http.StatusOK, dataEnvelope[[]Tweet]{Data: nil})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.SearchRecent(context.Background(), "cats", nil); err != nil {
		t.Fatalf("SearchRecent: %v", err)
	}
	if gotQuery != "query=cats" {
		t.Fatalf("expected only query=cats, got %q", gotQuery)
	}
}

func TestGetMentionsPagesOnPaginationToken(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		writeJSON(w, http.StatusOK, dataEnvelope[[]Tweet]{
			Data: []Tweet{{ID: "7", Text: "@me hi"}},
			Meta: &PageMeta{ResultCount: 1, NextToken: "MN"},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	page, err := c.GetMentions(context.Background(), "42", &TimelineParams{PaginationToken: "CUR"})
	if err != nil {
		t.Fatalf("GetMentions: %v", err)
	}
	if gotPath != "/2/users/42/mentions" {
		t.Fatalf("path = %q", gotPath)
	}
	if page.Meta == nil || page.Meta.NextToken != "MN" {
		t.Fatalf("expected meta.next_token=MN, got %#v", page.Meta)
	}
	if !strings.Contains(gotQuery, "pagination_token=CUR") {
		t.Fatalf("expected pagination_token in request, got %q", gotQuery)
	}
	if strings.Contains(gotQuery, "next_token=") {
		t.Fatalf("request must not send next_token: %q", gotQuery)
	}
}

func TestGetHomeTimelinePath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(w, http.StatusOK, dataEnvelope[[]Tweet]{Data: []Tweet{{ID: "1"}}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.GetHomeTimeline(context.Background(), "42", nil); err != nil {
		t.Fatalf("GetHomeTimeline: %v", err)
	}
	if gotPath != "/2/users/42/timelines/reverse_chronological" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestGetLikingUsersParsesUserPage(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		writeJSON(w, http.StatusOK, dataEnvelope[[]User]{
			Data: []User{{ID: "1", Username: "a"}, {ID: "2", Username: "b"}},
			Meta: &PageMeta{ResultCount: 2, NextToken: "LN"},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	page, err := c.GetLikingUsers(context.Background(), "1455", &PageParams{MaxResults: 100, PaginationToken: "CUR"})
	if err != nil {
		t.Fatalf("GetLikingUsers: %v", err)
	}
	if gotPath != "/2/tweets/1455/liking_users" {
		t.Fatalf("path = %q", gotPath)
	}
	if len(page.Users) != 2 || page.Meta == nil || page.Meta.NextToken != "LN" {
		t.Fatalf("unexpected page: users=%#v meta=%#v", page.Users, page.Meta)
	}
	if !strings.Contains(gotQuery, "pagination_token=CUR") || !strings.Contains(gotQuery, "max_results=100") {
		t.Fatalf("expected pagination_token+max_results, got %q", gotQuery)
	}
}

func TestGetRetweetedByPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(w, http.StatusOK, dataEnvelope[[]User]{Data: []User{{ID: "1"}}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.GetRetweetedBy(context.Background(), "1455", nil); err != nil {
		t.Fatalf("GetRetweetedBy: %v", err)
	}
	if gotPath != "/2/tweets/1455/retweeted_by" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestGetQuoteTweetsPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(w, http.StatusOK, dataEnvelope[[]Tweet]{Data: []Tweet{{ID: "1"}}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.GetQuoteTweets(context.Background(), "1455", nil); err != nil {
		t.Fatalf("GetQuoteTweets: %v", err)
	}
	if gotPath != "/2/tweets/1455/quote_tweets" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestAudienceReadsValidateEmptyID(t *testing.T) {
	c := newTestClient("http://twitter.test")
	if _, err := c.GetMentions(context.Background(), "", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("GetMentions empty id: %v", err)
	}
	if _, err := c.GetHomeTimeline(context.Background(), "", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("GetHomeTimeline empty id: %v", err)
	}
	if _, err := c.GetLikingUsers(context.Background(), "", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("GetLikingUsers empty id: %v", err)
	}
	if _, err := c.GetRetweetedBy(context.Background(), "", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("GetRetweetedBy empty id: %v", err)
	}
	if _, err := c.GetQuoteTweets(context.Background(), "", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("GetQuoteTweets empty id: %v", err)
	}
}
