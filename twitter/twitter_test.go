package twitter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (errorReader) Close() error             { return nil }

// newTestClient returns a *Client pointed at srvURL with a no-op logger and a
// fast stub sleeper so poll/retry tests don't sleep real wall-clock time.
func newTestClient(srvURL string) *Client {
	return &Client{
		httpClient:  http.DefaultClient,
		logger:      zap.NewNop(),
		accessToken: "test-token",
		baseURL:     srvURL,
		sleeper: func(ctx context.Context, _ time.Duration) bool {
			select {
			case <-ctx.Done():
				return false
			default:
				return true
			}
		},
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ── Auth / header plumbing ────────────────────────────────────────────────────

func TestCreateTweetPostsBodyAndParsesData(t *testing.T) {
	var gotAuth, gotPath, gotMethod, gotCT string
	var gotReq CreateTweetRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		writeJSON(w, http.StatusCreated, dataEnvelope[Tweet]{Data: Tweet{ID: "1455", Text: "hi"}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	tw, err := c.CreateTweet(context.Background(), &CreateTweetRequest{Text: "hi"})
	if err != nil {
		t.Fatalf("CreateTweet: %v", err)
	}
	if tw.ID != "1455" || tw.Text != "hi" {
		t.Fatalf("unexpected tweet: %#v", tw)
	}
	// On create the optional fields are zero.
	if tw.AuthorID != "" || tw.CreatedAt != "" || tw.PublicMetrics != nil {
		t.Fatalf("expected zero optional fields on create: %#v", tw)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotMethod != http.MethodPost || gotPath != "/2/tweets" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotCT != "application/json" {
		t.Fatalf("content-type = %q", gotCT)
	}
	if gotReq.Text != "hi" {
		t.Fatalf("server saw req %#v", gotReq)
	}
}

func TestCreateTweetValidatesEmpty(t *testing.T) {
	var called int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.CreateTweet(context.Background(), &CreateTweetRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
	if _, err := c.CreateTweet(context.Background(), nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest for nil, got %v", err)
	}
	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("HTTP endpoint called despite invalid request")
	}
}

func TestCreateTweetReplyOnlyAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, dataEnvelope[Tweet]{Data: Tweet{ID: "9", Text: ""}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	tw, err := c.CreateTweet(context.Background(), &CreateTweetRequest{
		Reply: &Reply{InReplyToTweetID: "123"},
	})
	if err != nil {
		t.Fatalf("reply-only request should be accepted: %v", err)
	}
	if tw.ID != "9" {
		t.Fatalf("unexpected tweet: %#v", tw)
	}
}

func TestDeleteTweetParsesDeleted(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		writeJSON(w, http.StatusOK, dataEnvelope[deleteResult]{Data: deleteResult{Deleted: true}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	ok, err := c.DeleteTweet(context.Background(), "1455")
	if err != nil {
		t.Fatalf("DeleteTweet: %v", err)
	}
	if !ok {
		t.Fatal("expected deleted=true")
	}
	if gotMethod != http.MethodDelete || gotPath != "/2/tweets/1455" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
}

// ── User reads ────────────────────────────────────────────────────────────────

func TestMeParsesUserWithMetrics(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		writeJSON(w, http.StatusOK, dataEnvelope[User]{Data: User{
			ID: "1", Name: "Ada", Username: "ada",
			PublicMetrics: &UserMetrics{FollowersCount: 10, TweetCount: 5},
		}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	u, err := c.Me(context.Background(), WithUserFields("public_metrics", "profile_image_url"))
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if u.Username != "ada" || u.PublicMetrics == nil || u.PublicMetrics.FollowersCount != 10 {
		t.Fatalf("unexpected user: %#v", u)
	}
	if !strings.Contains(gotQuery, "user.fields=public_metrics%2Cprofile_image_url") {
		t.Fatalf("expected single comma-joined user.fields, got %q", gotQuery)
	}
}

func TestGetUserByUsernameSingleFieldKey(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		writeJSON(w, http.StatusOK, dataEnvelope[User]{Data: User{ID: "2", Name: "Bo", Username: "bo"}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	// Two opts of the same kind must union into a single key, not duplicate.
	if _, err := c.GetUserByUsername(context.Background(), "bo",
		WithUserFields("a"), WithUserFields("b", "c")); err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if gotPath != "/2/users/by/username/bo" {
		t.Fatalf("path = %q", gotPath)
	}
	if strings.Count(gotQuery, "user.fields=") != 1 {
		t.Fatalf("expected single user.fields key, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "user.fields=b%2Cc") {
		t.Fatalf("expected later opt to replace earlier, got %q", gotQuery)
	}
}

func TestGetUsersJoinsIDs(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		writeJSON(w, http.StatusOK, dataEnvelope[[]User]{Data: []User{{ID: "1"}, {ID: "2"}}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	users, err := c.GetUsers(context.Background(), []string{"1", "2", "3"})
	if err != nil {
		t.Fatalf("GetUsers: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
	if strings.Count(gotQuery, "ids=") != 1 || !strings.Contains(gotQuery, "ids=1%2C2%2C3") {
		t.Fatalf("expected single comma-joined ids, got %q", gotQuery)
	}
}

func TestGetUsersEmptyRejected(t *testing.T) {
	c := newTestClient("http://twitter.test")
	if _, err := c.GetUsers(context.Background(), nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
}

func TestGetTweetsEmptyRejected(t *testing.T) {
	c := newTestClient("http://twitter.test")
	if _, err := c.GetTweets(context.Background(), []string{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
}

func TestGetUserTweetsParsesPageAndCursor(t *testing.T) {
	var gotQuery, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotPath = r.URL.RawQuery, r.URL.Path
		writeJSON(w, http.StatusOK, dataEnvelope[[]Tweet]{
			Data: []Tweet{{ID: "1", Text: "a"}, {ID: "2", Text: "b"}},
			Meta: &PageMeta{ResultCount: 2, NextToken: "NEXT123"},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	page, err := c.GetUserTweets(context.Background(), "42", &TimelineParams{
		MaxResults:      10,
		PaginationToken: "CURSOR0",
		Exclude:         []string{"retweets", "replies"},
		Fields:          []FieldOpt{WithTweetFields("public_metrics")},
	})
	if err != nil {
		t.Fatalf("GetUserTweets: %v", err)
	}
	if gotPath != "/2/users/42/tweets" {
		t.Fatalf("path = %q", gotPath)
	}
	if len(page.Tweets) != 2 || page.Meta == nil || page.Meta.NextToken != "NEXT123" {
		t.Fatalf("unexpected page: %#v meta=%#v", page.Tweets, page.Meta)
	}
	// Request sends pagination_token, NOT next_token.
	if !strings.Contains(gotQuery, "pagination_token=CURSOR0") {
		t.Fatalf("expected pagination_token in query, got %q", gotQuery)
	}
	if strings.Contains(gotQuery, "next_token=") {
		t.Fatalf("request must not send next_token: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "exclude=retweets%2Creplies") {
		t.Fatalf("expected comma-joined exclude, got %q", gotQuery)
	}
}

func TestGetUserTweetsNilParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("expected empty query for nil params, got %q", r.URL.RawQuery)
		}
		writeJSON(w, http.StatusOK, dataEnvelope[[]Tweet]{Data: nil})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	page, err := c.GetUserTweets(context.Background(), "42", nil)
	if err != nil {
		t.Fatalf("GetUserTweets: %v", err)
	}
	if len(page.Tweets) != 0 {
		t.Fatalf("expected no tweets, got %d", len(page.Tweets))
	}
}

// ── Missing-data rule ─────────────────────────────────────────────────────────

func TestMissingDataReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 2xx with only errors[] and absent data (e.g. nonexistent user).
		writeJSON(w, http.StatusOK, dataEnvelope[User]{
			Errors: []Problem{{Title: "Not Found Error", Detail: "Could not find user", Type: "about:blank", Status: 200}},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.GetUser(context.Background(), "999")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T %v", err, err)
	}
	if apiErr.StatusCode != http.StatusOK || apiErr.Title != "Not Found Error" {
		t.Fatalf("unexpected APIError: %#v", apiErr)
	}
	if len(apiErr.Problems) != 1 {
		t.Fatalf("expected problems populated, got %#v", apiErr.Problems)
	}
}

// ── Error handling ────────────────────────────────────────────────────────────

func TestAPIErrorPredicates(t *testing.T) {
	cases := []struct {
		code                    int
		rate, unauth, forbidden bool
	}{
		{http.StatusTooManyRequests, true, false, false},
		{http.StatusUnauthorized, false, true, false},
		{http.StatusForbidden, false, false, true},
		{http.StatusBadRequest, false, false, false},
	}
	for _, tc := range cases {
		e := &APIError{StatusCode: tc.code}
		if e.IsRateLimit() != tc.rate || e.IsUnauthorized() != tc.unauth || e.IsForbidden() != tc.forbidden {
			t.Fatalf("predicates wrong for %d: %#v", tc.code, e)
		}
	}
}

func TestFourxxParsedAndNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      int
		body      any
		wantTitle string
		checkPred func(*APIError) bool
	}{
		{
			name:      "top-level 401",
			code:      http.StatusUnauthorized,
			body:      problemBody{Title: "Unauthorized", Detail: "bad token", Type: "about:blank"},
			wantTitle: "Unauthorized",
			checkPred: (*APIError).IsUnauthorized,
		},
		{
			name:      "enveloped 429",
			code:      http.StatusTooManyRequests,
			body:      map[string]any{"errors": []Problem{{Title: "Too Many Requests", Detail: "slow down"}}},
			wantTitle: "Too Many Requests",
			checkPred: (*APIError).IsRateLimit,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&attempts, 1)
				writeJSON(w, tc.code, tc.body)
			}))
			defer srv.Close()

			c := newTestClient(srv.URL)
			_, err := c.GetTweet(context.Background(), "1")
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("expected *APIError, got %T %v", err, err)
			}
			if apiErr.StatusCode != tc.code || apiErr.Title != tc.wantTitle {
				t.Fatalf("unexpected APIError: %#v", apiErr)
			}
			if !tc.checkPred(apiErr) {
				t.Fatalf("predicate failed for %#v", apiErr)
			}
			if n := atomic.LoadInt32(&attempts); n != 1 {
				t.Fatalf("4xx must not be retried, got %d attempts", n)
			}
		})
	}
}

func TestRetryResendsBodyJSON(t *testing.T) {
	var attempts int32
	bodies := make([]string, 0, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, dataEnvelope[Tweet]{Data: Tweet{ID: "1", Text: "hi"}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.CreateTweet(context.Background(), &CreateTweetRequest{Text: "hi"}); err != nil {
		t.Fatalf("CreateTweet: %v", err)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if len(bodies) != 2 || bodies[0] == "" || bodies[0] != bodies[1] {
		t.Fatalf("retry body mismatch: %q vs %q", bodies[0], bodies[1])
	}
}

func TestDoRequestErrorBranches(t *testing.T) {
	c := newTestClient("://bad-url")
	if _, err := c.doRequest(context.Background(), "GET", "/x", nil, ""); err == nil {
		t.Fatal("expected request creation error")
	}

	c = newTestClient("http://twitter.test")
	c.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network")
	})}
	if _, err := c.doRequest(context.Background(), "GET", "/x", nil, ""); err == nil || !strings.Contains(err.Error(), "request failed after retries") {
		t.Fatalf("expected network retry error, got %v", err)
	}

	// 5xx exhausts all 3 attempts.
	var attempts int32
	c.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&attempts, 1)
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	if _, err := c.doRequest(context.Background(), "GET", "/x", nil, ""); err == nil || !strings.Contains(err.Error(), "request failed after retries") {
		t.Fatalf("expected retry exhaustion, got %v", err)
	}
	if atomic.LoadInt32(&attempts) != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}

	// 4xx with non-JSON body falls back to HTTP %d.
	c.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader("not-json"))}, nil
	})}
	if _, err := c.doRequest(context.Background(), "GET", "/x", nil, ""); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("expected HTTP 400 fallback, got %v", err)
	}
}

func TestBodyReadErrorRetriesWithoutSleeping(t *testing.T) {
	var attempts int32
	c := newTestClient("http://twitter.test")
	c.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&attempts, 1)
		return &http.Response{StatusCode: http.StatusOK, Body: errorReader{}}, nil
	})}

	start := time.Now()
	_, err := c.doRequest(context.Background(), "GET", "/x", nil, "")
	if err == nil || !strings.Contains(err.Error(), "request failed after retries") {
		t.Fatalf("expected retry exhaustion, got %v", err)
	}
	if atomic.LoadInt32(&attempts) != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
	// Body-read errors continue without sleeping; 3 attempts should be ~instant.
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("body-read retries should not sleep, took %v", elapsed)
	}
}

// ── Media upload ──────────────────────────────────────────────────────────────

func mediaEnvelope(m Media) []byte {
	b, _ := json.Marshal(dataEnvelope[Media]{Data: m})
	return b
}

func TestUploadMediaHappyPath(t *testing.T) {
	const payloadSize = chunkSize + 1024 // forces 2 segments
	payload := bytes.Repeat([]byte("x"), payloadSize)

	var (
		initTotalBytes int
		segments       []string
		appendBodies   [][]byte
		statusCalls    int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/2/media/upload/initialize":
			var req initMediaRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			initTotalBytes = req.TotalBytes
			w.Write(mediaEnvelope(Media{ID: "MID"}))
		case strings.HasSuffix(r.URL.Path, "/append"):
			body, _ := io.ReadAll(r.Body)
			appendBodies = append(appendBodies, body)
			seg := parseSegmentIndex(t, r, body)
			segments = append(segments, seg)
			w.Write(mediaEnvelope(Media{}))
		case strings.HasSuffix(r.URL.Path, "/finalize"):
			w.Write(mediaEnvelope(Media{ID: "MID", ProcessingInfo: &ProcessingInfo{State: "in_progress", CheckAfterSecs: 1}}))
		case r.URL.Path == "/2/media/upload" && r.URL.Query().Get("command") == "STATUS":
			n := atomic.AddInt32(&statusCalls, 1)
			state := "in_progress"
			if n >= 2 {
				state = "succeeded"
			}
			w.Write(mediaEnvelope(Media{ID: "MID", ProcessingInfo: &ProcessingInfo{State: state}}))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	m, err := c.UploadMedia(context.Background(), payload, "image/jpeg", "tweet_image")
	if err != nil {
		t.Fatalf("UploadMedia: %v", err)
	}
	if m.ID != "MID" {
		t.Fatalf("unexpected media: %#v", m)
	}
	if initTotalBytes != payloadSize {
		t.Fatalf("total_bytes = %d, want %d", initTotalBytes, payloadSize)
	}
	if len(segments) != 2 {
		t.Fatalf("expected 2 segments, got %d (%v)", len(segments), segments)
	}
	if segments[0] != "0" || segments[1] != "1" {
		t.Fatalf("segment indices = %v, want [0 1]", segments)
	}
	if atomic.LoadInt32(&statusCalls) != 2 {
		t.Fatalf("expected 2 STATUS polls, got %d", statusCalls)
	}
}

// parseSegmentIndex extracts the segment_index field from a multipart append body.
func parseSegmentIndex(t *testing.T, r *http.Request, body []byte) string {
	t.Helper()
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse content-type: %v", err)
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var seg string
	var sawMedia bool
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("next part: %v", err)
		}
		switch part.FormName() {
		case "segment_index":
			b, _ := io.ReadAll(part)
			seg = string(b)
		case "media":
			sawMedia = true
			if part.FileName() == "" {
				t.Error("media part missing filename")
			}
		}
	}
	if !sawMedia {
		t.Error("append body missing media part")
	}
	return seg
}

func TestUploadMediaSyncImagePath(t *testing.T) {
	// finalize returns no processing_info → return immediately, no STATUS poll.
	var statusCalled int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/2/media/upload/initialize":
			w.Write(mediaEnvelope(Media{ID: "IMG"}))
		case strings.HasSuffix(r.URL.Path, "/append"):
			w.Write(mediaEnvelope(Media{}))
		case strings.HasSuffix(r.URL.Path, "/finalize"):
			w.Write(mediaEnvelope(Media{ID: "IMG"})) // nil ProcessingInfo
		case r.URL.Query().Get("command") == "STATUS":
			atomic.AddInt32(&statusCalled, 1)
			w.Write(mediaEnvelope(Media{ID: "IMG"}))
		}
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	m, err := c.UploadMedia(context.Background(), []byte("small"), "image/jpeg", "tweet_image")
	if err != nil {
		t.Fatalf("UploadMedia: %v", err)
	}
	if m.ID != "IMG" {
		t.Fatalf("unexpected media: %#v", m)
	}
	if atomic.LoadInt32(&statusCalled) != 0 {
		t.Fatal("nil ProcessingInfo must not trigger STATUS poll")
	}
}

func TestUploadMediaFailedState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/2/media/upload/initialize":
			w.Write(mediaEnvelope(Media{ID: "VID"}))
		case strings.HasSuffix(r.URL.Path, "/append"):
			w.Write(mediaEnvelope(Media{}))
		case strings.HasSuffix(r.URL.Path, "/finalize"):
			w.Write(mediaEnvelope(Media{ID: "VID", ProcessingInfo: &ProcessingInfo{State: "failed"}}))
		}
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.UploadMedia(context.Background(), []byte("data"), "video/mp4", "tweet_video")
	if err == nil || !strings.Contains(err.Error(), "VID") {
		t.Fatalf("expected failure naming media id VID, got %v", err)
	}
}

func TestUploadMediaValidatesArgs(t *testing.T) {
	c := newTestClient("http://twitter.test")
	if _, err := c.UploadMedia(context.Background(), nil, "image/jpeg", "tweet_image"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty data: %v", err)
	}
	if _, err := c.UploadMedia(context.Background(), []byte("x"), "image/jpeg", "bad_category"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("bad category: %v", err)
	}
}

func TestAppendRetryResendsIdenticalMultipart(t *testing.T) {
	var appendAttempts int32
	bodies := make([][]byte, 0, 2)
	cts := make([]string, 0, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/2/media/upload/initialize":
			w.Write(mediaEnvelope(Media{ID: "MID"}))
		case strings.HasSuffix(r.URL.Path, "/append"):
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, body)
			cts = append(cts, r.Header.Get("Content-Type"))
			if atomic.AddInt32(&appendAttempts, 1) == 1 {
				w.WriteHeader(http.StatusInternalServerError) // force retry
				return
			}
			w.Write(mediaEnvelope(Media{}))
		case strings.HasSuffix(r.URL.Path, "/finalize"):
			w.Write(mediaEnvelope(Media{ID: "MID"}))
		}
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.UploadMedia(context.Background(), []byte("chunk"), "image/jpeg", "tweet_image"); err != nil {
		t.Fatalf("UploadMedia: %v", err)
	}
	if atomic.LoadInt32(&appendAttempts) != 2 {
		t.Fatalf("expected 2 append attempts, got %d", appendAttempts)
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("retry must resend identical multipart bytes")
	}
	if cts[0] != cts[1] || !strings.HasPrefix(cts[0], "multipart/form-data") {
		t.Fatalf("content-type must be a stable multipart type: %q vs %q", cts[0], cts[1])
	}
}

func TestSetMediaMetadataPostsNestedBody(t *testing.T) {
	var got metadataCreateRequest
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	if err := c.SetMediaMetadata(context.Background(), "MID", "a cat"); err != nil {
		t.Fatalf("SetMediaMetadata: %v", err)
	}
	if gotPath != "/2/media/metadata" {
		t.Fatalf("path = %q", gotPath)
	}
	if got.ID != "MID" || got.Metadata.AltText == nil || got.Metadata.AltText.Text != "a cat" {
		t.Fatalf("unexpected metadata body: %#v", got)
	}
}

func TestSetMediaMetadataRawShape(t *testing.T) {
	// Assert the exact JSON wire shape {id, metadata:{alt_text:{text}}}.
	c := &Client{}
	_ = c
	req := metadataCreateRequest{ID: "X", Metadata: metadataBody{AltText: &altText{Text: "hi"}}}
	b, _ := json.Marshal(req)
	want := `{"id":"X","metadata":{"alt_text":{"text":"hi"}}}`
	if string(b) != want {
		t.Fatalf("wire shape = %s, want %s", b, want)
	}
}

func TestNewReturnsClient(t *testing.T) {
	if New("token", zap.NewNop()) == nil {
		t.Fatal("expected client")
	}
}

func TestAPIErrorString(t *testing.T) {
	if got := (&APIError{StatusCode: 400, Title: "Bad", Detail: "x"}).Error(); !strings.Contains(got, "400") {
		t.Fatalf("unexpected: %q", got)
	}
	if got := (&APIError{StatusCode: 500}).Error(); !strings.Contains(got, "500") {
		t.Fatalf("unexpected: %q", got)
	}
	_ = fmt.Sprint(&APIError{})
}
