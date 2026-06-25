package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestLikePostsTweetIDAndParsesLiked(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody tweetIDBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, http.StatusOK, dataEnvelope[likeResult]{Data: likeResult{Liked: true}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	ok, err := c.Like(context.Background(), "42", "1455")
	if err != nil {
		t.Fatalf("Like: %v", err)
	}
	if !ok {
		t.Fatal("expected liked=true")
	}
	if gotMethod != http.MethodPost || gotPath != "/2/users/42/likes" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotBody.TweetID != "1455" {
		t.Fatalf("body tweet_id = %q", gotBody.TweetID)
	}
}

func TestLikeValidatesEmpty(t *testing.T) {
	var called int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.Like(context.Background(), "", "1"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty userID: %v", err)
	}
	if _, err := c.Like(context.Background(), "1", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty tweetID: %v", err)
	}
	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("HTTP endpoint called despite invalid request")
	}
}

func TestUnlikeDeletesTwoSegmentPathNoBody(t *testing.T) {
	var gotMethod, gotPath string
	var gotBodyLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBodyLen = r.ContentLength
		writeJSON(w, http.StatusOK, dataEnvelope[likeResult]{Data: likeResult{Liked: false}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	ok, err := c.Unlike(context.Background(), "42", "1455")
	if err != nil {
		t.Fatalf("Unlike: %v", err)
	}
	if ok {
		t.Fatal("expected liked=false after unlike")
	}
	if gotMethod != http.MethodDelete || gotPath != "/2/users/42/likes/1455" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotBodyLen > 0 {
		t.Fatalf("DELETE must send no body, got content-length %d", gotBodyLen)
	}
}

func TestRetweetPostsTweetIDAndParsesRetweeted(t *testing.T) {
	var gotPath string
	var gotBody tweetIDBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, http.StatusOK, dataEnvelope[retweetResult]{Data: retweetResult{Retweeted: true}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	ok, err := c.Retweet(context.Background(), "42", "1455")
	if err != nil {
		t.Fatalf("Retweet: %v", err)
	}
	if !ok {
		t.Fatal("expected retweeted=true")
	}
	if gotPath != "/2/users/42/retweets" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody.TweetID != "1455" {
		t.Fatalf("body tweet_id = %q", gotBody.TweetID)
	}
}

func TestUnretweetDeletesSourceTweetPathNoBody(t *testing.T) {
	var gotMethod, gotPath string
	var gotBodyLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBodyLen = r.ContentLength
		writeJSON(w, http.StatusOK, dataEnvelope[retweetResult]{Data: retweetResult{Retweeted: false}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	ok, err := c.Unretweet(context.Background(), "42", "1455")
	if err != nil {
		t.Fatalf("Unretweet: %v", err)
	}
	if ok {
		t.Fatal("expected retweeted=false after unretweet")
	}
	if gotMethod != http.MethodDelete || gotPath != "/2/users/42/retweets/1455" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotBodyLen > 0 {
		t.Fatalf("DELETE must send no body, got content-length %d", gotBodyLen)
	}
}

func TestHideReplyPutsHiddenAndParsesHidden(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody hiddenBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, http.StatusOK, dataEnvelope[hideResult]{Data: hideResult{Hidden: true}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	ok, err := c.HideReply(context.Background(), "1455", true)
	if err != nil {
		t.Fatalf("HideReply: %v", err)
	}
	if !ok {
		t.Fatal("expected hidden=true")
	}
	if gotMethod != http.MethodPut || gotPath != "/2/tweets/1455/hidden" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if !gotBody.Hidden {
		t.Fatalf("body hidden = %v, want true", gotBody.Hidden)
	}
}

func TestHideReplyValidatesEmpty(t *testing.T) {
	var called int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.HideReply(context.Background(), "", true); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("HTTP endpoint called despite invalid request")
	}
}

func TestRetweetValidatesEmpty(t *testing.T) {
	c := newTestClient("http://twitter.test")
	if _, err := c.Retweet(context.Background(), "", "1"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty userID: %v", err)
	}
	if _, err := c.Unretweet(context.Background(), "1", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty tweetID: %v", err)
	}
	if _, err := c.Unlike(context.Background(), "", "1"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty userID: %v", err)
	}
}
