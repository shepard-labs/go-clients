package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateArticleDraftParsesArticle(t *testing.T) {
	var gotMethod, gotPath string
	var got ArticleDraftRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, http.StatusCreated, dataEnvelope[Article]{Data: Article{ID: "A1", Title: "My Article"}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	art, err := c.CreateArticleDraft(context.Background(), &ArticleDraftRequest{
		Title:        "My Article",
		ContentState: json.RawMessage(`{"blocks":[]}`),
	})
	if err != nil {
		t.Fatalf("CreateArticleDraft: %v", err)
	}
	if art.ID != "A1" || art.Title != "My Article" {
		t.Fatalf("unexpected article: %#v", art)
	}
	if gotMethod != http.MethodPost || gotPath != "/2/articles/draft" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	if got.Title != "My Article" || string(got.ContentState) != `{"blocks":[]}` {
		t.Fatalf("server saw req %#v", got)
	}
}

func TestCreateArticleDraftValidates(t *testing.T) {
	c := newTestClient("http://twitter.test")
	if _, err := c.CreateArticleDraft(context.Background(), nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil req: %v", err)
	}
	if _, err := c.CreateArticleDraft(context.Background(),
		&ArticleDraftRequest{ContentState: json.RawMessage(`{}`)}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing title: %v", err)
	}
	if _, err := c.CreateArticleDraft(context.Background(),
		&ArticleDraftRequest{Title: "T"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing content_state: %v", err)
	}
}

func TestPublishArticleReturnsPostID(t *testing.T) {
	var gotMethod, gotPath, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotCT = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		// Publish response is {post_id} only — NOT {id, title}.
		writeJSON(w, http.StatusOK, dataEnvelope[struct {
			PostID string `json:"post_id"`
		}]{Data: struct {
			PostID string `json:"post_id"`
		}{PostID: "P1"}})
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	postID, err := c.PublishArticle(context.Background(), "A1")
	if err != nil {
		t.Fatalf("PublishArticle: %v", err)
	}
	if postID != "P1" {
		t.Fatalf("expected post id P1, got %q", postID)
	}
	if gotMethod != http.MethodPost || gotPath != "/2/articles/A1/publish" {
		t.Fatalf("method/path = %s %s", gotMethod, gotPath)
	}
	// Publish sends no body.
	if gotBody != "" || gotCT != "" {
		t.Fatalf("publish must send no body and no Content-Type: body=%q ct=%q", gotBody, gotCT)
	}
}

func TestPublishArticleValidates(t *testing.T) {
	c := newTestClient("http://twitter.test")
	if _, err := c.PublishArticle(context.Background(), ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty articleID: %v", err)
	}
}

func TestArticleForbiddenSurfacesAsAPIError(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Client) error
	}{
		{"CreateArticleDraft", func(c *Client) error {
			_, err := c.CreateArticleDraft(context.Background(),
				&ArticleDraftRequest{Title: "T", ContentState: json.RawMessage(`{}`)})
			return err
		}},
		{"PublishArticle", func(c *Client) error {
			_, err := c.PublishArticle(context.Background(), "A1")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusForbidden, problemBody{
					Title: "Forbidden", Detail: "ineligible access tier", Type: "about:blank",
				})
			}))
			defer srv.Close()

			c := newTestClient(srv.URL)
			err := tc.call(c)
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("expected *APIError, got %T %v", err, err)
			}
			if !apiErr.IsForbidden() {
				t.Fatalf("expected IsForbidden, got %#v", apiErr)
			}
		})
	}
}
