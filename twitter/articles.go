package twitter

import (
	"context"
	"fmt"
	"net/http"
)

// Articles require X's elevated/eligible API access. A runtime 403
// (APIError.IsForbidden()) means the linked account/app lacks the access tier.

// CreateArticleDraft creates a long-form article draft and returns it. The
// draft response data is {id, title}. title and content_state are required.
// POST /2/articles/draft.
func (c *Client) CreateArticleDraft(ctx context.Context, req *ArticleDraftRequest) (*Article, error) {
	if req == nil || req.Title == "" || len(req.ContentState) == 0 {
		return nil, fmt.Errorf("%w: article title and content_state required", ErrInvalidRequest)
	}

	var env dataEnvelope[Article]
	if err := c.doJSON(ctx, http.MethodPost, "/2/articles/draft", req, &env); err != nil {
		return nil, err
	}
	return dataOrErr(&env)
}

// PublishArticle publishes a draft article and returns the id of the resulting
// post. This endpoint takes no body; its response data is {post_id} only (NOT
// an Article), so it does not route through dataOrErr.
// POST /2/articles/{article_id}/publish.
func (c *Client) PublishArticle(ctx context.Context, articleID string) (string, error) {
	if articleID == "" {
		return "", fmt.Errorf("%w: articleID required", ErrInvalidRequest)
	}

	var env dataEnvelope[struct {
		PostID string `json:"post_id"`
	}]
	if err := c.doJSON(ctx, http.MethodPost, "/2/articles/"+articleID+"/publish", nil, &env); err != nil {
		return "", err
	}
	return env.Data.PostID, nil
}
