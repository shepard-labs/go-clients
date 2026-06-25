package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetBookmarksReturnsTweetPage(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		writeJSON(w, http.StatusOK, dataEnvelope[[]Tweet]{
			Data: []Tweet{{ID: "1", Text: "a"}, {ID: "2", Text: "b"}},
			Meta: &PageMeta{ResultCount: 2, NextToken: "BNEXT"},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	page, err := c.GetBookmarks(context.Background(), "U1", &TimelineParams{MaxResults: 50, PaginationToken: "C0"})
	if err != nil {
		t.Fatalf("GetBookmarks: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/2/users/U1/bookmarks" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if len(page.Tweets) != 2 || page.Meta == nil || page.Meta.NextToken != "BNEXT" {
		t.Fatalf("unexpected page: %#v meta=%#v", page.Tweets, page.Meta)
	}
	if !strings.Contains(gotQuery, "pagination_token=C0") {
		t.Fatalf("expected pagination_token, got %q", gotQuery)
	}
	if strings.Contains(gotQuery, "next_token=") {
		t.Fatalf("request must not send next_token: %q", gotQuery)
	}
}

func TestAddBookmarkPostsTweetID(t *testing.T) {
	var gotMethod, gotPath string
	var got tweetIDBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, http.StatusOK, dataEnvelope[bookmarkResult]{Data: bookmarkResult{Bookmarked: true}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	ok, err := c.AddBookmark(context.Background(), "U1", "T2")
	if err != nil {
		t.Fatalf("AddBookmark: %v", err)
	}
	if !ok {
		t.Fatal("expected bookmarked=true")
	}
	if gotMethod != http.MethodPost || gotPath != "/2/users/U1/bookmarks" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if got.TweetID != "T2" {
		t.Fatalf("body = %#v, want tweet_id=T2", got)
	}
}

func TestRemoveBookmarkDeletesByExactPathNoBody(t *testing.T) {
	var gotMethod, gotPath, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotCT = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		writeJSON(w, http.StatusOK, dataEnvelope[bookmarkResult]{Data: bookmarkResult{Bookmarked: false}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	bookmarked, err := c.RemoveBookmark(context.Background(), "U1", "T2")
	if err != nil {
		t.Fatalf("RemoveBookmark: %v", err)
	}
	if bookmarked {
		t.Fatal("expected bookmarked=false on removal")
	}
	if gotMethod != http.MethodDelete || gotPath != "/2/users/U1/bookmarks/T2" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotBody != "" || gotCT != "" {
		t.Fatalf("DELETE must send no body and no Content-Type: body=%q ct=%q", gotBody, gotCT)
	}
}

func TestBookmarkWritersValidate(t *testing.T) {
	c := newTestClient("http://twitter.test")
	if _, err := c.AddBookmark(context.Background(), "", "T2"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("AddBookmark empty userID: %v", err)
	}
	if _, err := c.RemoveBookmark(context.Background(), "U1", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("RemoveBookmark empty tweetID: %v", err)
	}
	if _, err := c.GetBookmarks(context.Background(), "", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("GetBookmarks empty userID: %v", err)
	}
}
