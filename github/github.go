// Package github implements a low-level GitHub App REST client.
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
)

// DefaultBaseURL is GitHub's public API endpoint.
const DefaultBaseURL = "https://api.github.com"

// AppClient is the low-level GitHub App REST client.
type AppClient struct {
	httpClient  *http.Client
	baseURL     string
	appID       int64
	appSlug     string
	privateKey  *rsa.PrivateKey
	jwtAudience string
	logger      *zap.Logger
}

// NewAppClient constructs a GitHub App client.
func NewAppClient(baseURL string, appID int64, appSlug, privateKeyPEM string, logger *zap.Logger) (*AppClient, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(privateKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("failed to parse GitHub App private key: %w", err)
	}
	return &AppClient{
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		baseURL:     strings.TrimRight(baseURL, "/"),
		appID:       appID,
		appSlug:     appSlug,
		privateKey:  key,
		jwtAudience: "github-app",
		logger:      logger,
	}, nil
}

// InstallationAccessToken is a short-lived installation token.
type InstallationAccessToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Installation represents a GitHub App installation.
type Installation struct {
	ID      int64   `json:"id"`
	Account Account `json:"account"`
	HTMLURL string  `json:"html_url"`
}

// Account is the account that owns the installation.
type Account struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

// Repository is a repository accessible to an installation.
type Repository struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	FullName      string  `json:"full_name"`
	Owner         Account `json:"owner"`
	Private       bool    `json:"private"`
	DefaultBranch string  `json:"default_branch"`
}

// Branch is a branch reference.
type Branch struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

// PullRequest is a GitHub pull request.
type PullRequest struct {
	ID      int64  `json:"id"`
	Number  int64  `json:"number"`
	URL     string `json:"url"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Title   string `json:"title"`
	Head    struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"base"`
	Body   string `json:"body"`
	Merged bool   `json:"merged"`
}

// CreatePullRequest is the payload for creating a PR.
type CreatePullRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
}

// UpdatePullRequest is the payload for updating a PR.
type UpdatePullRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// ContentFile is a file returned by the Contents API.
type ContentFile struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
	Type     string `json:"type"`
	URL      string `json:"url"`
}

// CreateOrUpdateFileRequest is the payload for writing a file.
type CreateOrUpdateFileRequest struct {
	Message string `json:"message"`
	Content string `json:"content"`
	Branch  string `json:"branch,omitempty"`
	SHA     string `json:"sha,omitempty"`
}

// CreateOrUpdateFileResponse is the response from writing a file.
type CreateOrUpdateFileResponse struct {
	Content struct {
		SHA string `json:"sha"`
	} `json:"content"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

// Error is a GitHub API error with HTTP status and optional rate limit info.
type Error struct {
	StatusCode     int
	Message        string
	RetryAfter     time.Time
	RateLimitReset time.Time
	RequestID      string
	Body           string
}

func (e *Error) Error() string {
	return fmt.Sprintf("github api error: status=%d message=%s", e.StatusCode, e.Message)
}

// IsTransient returns true for timeouts, 5xx, 429, secondary rate limits, and ref races.
func (e *Error) IsTransient() bool {
	if e.StatusCode >= 500 && e.StatusCode <= 599 || e.StatusCode == 429 || e.IsRateLimited() {
		return true
	}
	return (e.StatusCode == 409 || e.StatusCode == 422) && (e.IsRefRace() || e.IsAlreadyExists())
}

// IsPermanent returns true for auth/permission/repository errors that won't fix themselves.
func (e *Error) IsPermanent() bool {
	if e.StatusCode == 401 || e.StatusCode == 404 {
		return true
	}
	if e.StatusCode == 403 && !e.IsRateLimited() {
		return true
	}
	return e.StatusCode == 422 && !e.IsRefRace() && !e.IsAlreadyExists()
}

// IsRateLimited returns true for 429 or secondary-rate-limit 403s.
func (e *Error) IsRateLimited() bool {
	return e.StatusCode == 429 || e.StatusCode == 403 && strings.Contains(strings.ToLower(e.Message), "secondary rate limit")
}

// IsRefRace reports whether the error is a git ref race.
func (e *Error) IsRefRace() bool {
	if e.StatusCode != 409 && e.StatusCode != 422 {
		return false
	}
	msg := strings.ToLower(e.Message)
	return strings.Contains(msg, "reference already exists") || strings.Contains(msg, "ref already exists") || strings.Contains(msg, "already exists")
}

// IsAlreadyExists reports whether the error is an "already exists" conflict.
func (e *Error) IsAlreadyExists() bool {
	return strings.Contains(strings.ToLower(e.Message), "already exists")
}

// IsPermissionDenied reports whether the error is a permission/auth failure.
func (e *Error) IsPermissionDenied() bool {
	msg := strings.ToLower(e.Message)
	return strings.Contains(msg, "permission") || strings.Contains(msg, "forbidden") || strings.Contains(msg, "access denied") || strings.Contains(msg, "resource not accessible")
}

// InstallationToken creates a JWT and exchanges it for an installation token.
func (c *AppClient) InstallationToken(ctx context.Context, installationID int64) (*InstallationAccessToken, error) {
	token, err := c.appJWT()
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/app/installations/%d/access_tokens", c.baseURL, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	c.setAppHeaders(req, token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return nil, errorFromResponse(resp, body)
	}
	var out InstallationAccessToken
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("failed to parse installation token response: %w", err)
	}
	return &out, nil
}

// Installation fetches an installation by ID.
func (c *AppClient) Installation(ctx context.Context, installationID int64) (*Installation, error) {
	body, err := c.doAppJWTRequest(ctx, http.MethodGet, fmt.Sprintf("%s/app/installations/%d", c.baseURL, installationID), nil)
	if err != nil {
		return nil, err
	}
	var out Installation
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("failed to parse installation: %w", err)
	}
	return &out, nil
}

// InstallationRepositories lists repositories accessible to an installation.
func (c *AppClient) InstallationRepositories(ctx context.Context, installationID int64, page, perPage int) ([]Repository, error) {
	if perPage <= 0 || perPage > 100 {
		perPage = 100
	}
	body, err := c.doInstallationRequest(ctx, http.MethodGet, fmt.Sprintf("%s/installation/repositories?page=%d&per_page=%d", c.baseURL, page, perPage), installationID, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Repositories []Repository `json:"repositories"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("failed to parse installation repositories: %w", err)
	}
	return out.Repositories, nil
}

// ListBranches returns a page of branches for a repository.
func (c *AppClient) ListBranches(ctx context.Context, installationID, repoID int64, cursor string, perPage int) ([]Branch, string, error) {
	if perPage <= 0 || perPage > 100 {
		perPage = 100
	}
	u := fmt.Sprintf("%s/repositories/%d/branches?per_page=%d", c.baseURL, repoID, perPage)
	if cursor != "" {
		u += "&after=" + url.QueryEscape(cursor)
	}
	body, headers, err := c.doInstallationRequestWithHeaders(ctx, http.MethodGet, u, installationID, nil)
	if err != nil {
		return nil, "", err
	}
	var branches []Branch
	if err := json.Unmarshal(body, &branches); err != nil {
		return nil, "", fmt.Errorf("failed to parse branches: %w", err)
	}
	return branches, nextCursorFromLink(headers.Get("Link")), nil
}

// GetBranch fetches a branch ref.
func (c *AppClient) GetBranch(ctx context.Context, installationID, repoID int64, branch string) (*Branch, error) {
	body, err := c.doInstallationRequest(ctx, http.MethodGet, fmt.Sprintf("%s/repositories/%d/branches/%s", c.baseURL, repoID, url.PathEscape(branch)), installationID, nil)
	if err != nil {
		return nil, err
	}
	var out Branch
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("failed to parse branch: %w", err)
	}
	return &out, nil
}

// GetRepository fetches a repository by ID.
func (c *AppClient) GetRepository(ctx context.Context, installationID, repoID int64) (*Repository, error) {
	body, err := c.doInstallationRequest(ctx, http.MethodGet, fmt.Sprintf("%s/repositories/%d", c.baseURL, repoID), installationID, nil)
	if err != nil {
		return nil, err
	}
	var out Repository
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("failed to parse repository: %w", err)
	}
	return &out, nil
}

// GetFile returns the file on a branch.
func (c *AppClient) GetFile(ctx context.Context, installationID, repoID int64, path, ref string) (*ContentFile, error) {
	body, err := c.doInstallationRequest(ctx, http.MethodGet, fmt.Sprintf("%s/repositories/%d/contents/%s?ref=%s", c.baseURL, repoID, url.PathEscape(path), url.QueryEscape(ref)), installationID, nil)
	if err != nil {
		return nil, err
	}
	var out ContentFile
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("failed to parse file: %w", err)
	}
	return &out, nil
}

// PutFile creates or updates a file on a branch.
func (c *AppClient) PutFile(ctx context.Context, installationID, repoID int64, path, branch, message string, content []byte, sha string) (*CreateOrUpdateFileResponse, error) {
	payload := CreateOrUpdateFileRequest{Message: message, Content: base64.StdEncoding.EncodeToString(content), Branch: branch, SHA: sha}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	respBody, err := c.doInstallationRequest(ctx, http.MethodPut, fmt.Sprintf("%s/repositories/%d/contents/%s", c.baseURL, repoID, url.PathEscape(path)), installationID, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out CreateOrUpdateFileResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("failed to parse file write response: %w", err)
	}
	return &out, nil
}

// CreatePR creates a pull request.
func (c *AppClient) CreatePR(ctx context.Context, installationID, repoID int64, payload CreatePullRequest) (*PullRequest, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	respBody, err := c.doInstallationRequest(ctx, http.MethodPost, fmt.Sprintf("%s/repositories/%d/pulls", c.baseURL, repoID), installationID, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out PullRequest
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("failed to parse pull request: %w", err)
	}
	return &out, nil
}

// GetPR fetches a pull request by number.
func (c *AppClient) GetPR(ctx context.Context, installationID, repoID, number int64) (*PullRequest, error) {
	body, err := c.doInstallationRequest(ctx, http.MethodGet, fmt.Sprintf("%s/repositories/%d/pulls/%d", c.baseURL, repoID, number), installationID, nil)
	if err != nil {
		return nil, err
	}
	var out PullRequest
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("failed to parse pull request: %w", err)
	}
	return &out, nil
}

// ListOpenPRs returns open PRs filtered by head branch name.
func (c *AppClient) ListOpenPRs(ctx context.Context, installationID, repoID int64, headBranch string, perPage int) ([]PullRequest, error) {
	if perPage <= 0 || perPage > 100 {
		perPage = 100
	}
	body, err := c.doInstallationRequest(ctx, http.MethodGet, fmt.Sprintf("%s/repositories/%d/pulls?state=open&head=%s&per_page=%d", c.baseURL, repoID, url.QueryEscape(headBranch), perPage), installationID, nil)
	if err != nil {
		return nil, err
	}
	var prs []PullRequest
	if err := json.Unmarshal(body, &prs); err != nil {
		return nil, fmt.Errorf("failed to parse pull requests: %w", err)
	}
	return prs, nil
}

// FindOpenPRByHead searches open PRs by head branch and optional base.
func (c *AppClient) FindOpenPRByHead(ctx context.Context, installationID, repoID int64, headBranch, baseBranch string) (*PullRequest, error) {
	prs, err := c.ListOpenPRs(ctx, installationID, repoID, headBranch, 100)
	if err != nil {
		return nil, err
	}
	for _, pr := range prs {
		if pr.Head.Ref == headBranch && (baseBranch == "" || pr.Base.Ref == baseBranch) {
			return &pr, nil
		}
	}
	return nil, nil
}

// CreateBranch creates a new branch from an existing SHA.
func (c *AppClient) CreateBranch(ctx context.Context, installationID, repoID int64, name, sha string) (*Branch, error) {
	payload := map[string]string{"ref": fmt.Sprintf("refs/heads/%s", name), "sha": sha}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	respBody, err := c.doInstallationRequest(ctx, http.MethodPost, fmt.Sprintf("%s/repositories/%d/git/refs", c.baseURL, repoID), installationID, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("failed to parse branch: %w", err)
	}
	b := &Branch{Name: strings.TrimPrefix(out.Ref, "refs/heads/")}
	b.Commit.SHA = out.Object.SHA
	return b, nil
}

// UpdatePR updates a pull request.
func (c *AppClient) UpdatePR(ctx context.Context, installationID, repoID, number int64, payload UpdatePullRequest) (*PullRequest, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	respBody, err := c.doInstallationRequest(ctx, http.MethodPatch, fmt.Sprintf("%s/repositories/%d/pulls/%d", c.baseURL, repoID, number), installationID, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var out PullRequest
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("failed to parse pull request update: %w", err)
	}
	return &out, nil
}

func (c *AppClient) appJWT() (string, error) {
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": c.appID, "aud": c.jwtAudience})
	tokenString, err := token.SignedString(c.privateKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign JWT: %w", err)
	}
	return tokenString, nil
}
func (c *AppClient) setAppHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "spiderreach-backend")
}
func (c *AppClient) doAppJWTRequest(ctx context.Context, method, requestURL string, body io.Reader) ([]byte, error) {
	token, err := c.appJWT()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return nil, err
	}
	c.setAppHeaders(req, token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errorFromResponse(resp, respBody)
	}
	return respBody, nil
}
func (c *AppClient) doInstallationRequest(ctx context.Context, method, requestURL string, installationID int64, body io.Reader) ([]byte, error) {
	data, _, err := c.doInstallationRequestWithHeaders(ctx, method, requestURL, installationID, body)
	return data, err
}
func (c *AppClient) doInstallationRequestWithHeaders(ctx context.Context, method, requestURL string, installationID int64, body io.Reader) ([]byte, http.Header, error) {
	token, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "spiderreach-backend")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, errorFromResponse(resp, respBody)
	}
	return respBody, resp.Header, nil
}
func nextCursorFromLink(link string) string {
	for _, part := range strings.Split(link, ",") {
		section := strings.Split(part, ";")
		if len(section) < 2 {
			continue
		}
		rawURL := strings.TrimSpace(strings.Trim(section[0], "<>"))
		if strings.TrimSpace(section[1]) == `rel="next"` {
			if parsed, err := url.Parse(rawURL); err == nil {
				return parsed.Query().Get("after")
			}
		}
	}
	return ""
}
func errorFromResponse(resp *http.Response, body []byte) *Error {
	e := &Error{StatusCode: resp.StatusCode, RequestID: resp.Header.Get("X-GitHub-Request-Id"), Body: string(body), Message: resp.Status}
	var ghErr struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &ghErr); err == nil && ghErr.Message != "" {
		e.Message = ghErr.Message
	}
	if retry := resp.Header.Get("Retry-After"); retry != "" {
		if seconds, err := strconv.Atoi(retry); err == nil {
			e.RetryAfter = time.Now().Add(time.Duration(seconds) * time.Second)
		}
	}
	if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
		if ts, err := strconv.ParseInt(reset, 10, 64); err == nil {
			e.RateLimitReset = time.Unix(ts, 0)
		}
	}
	return e
}

// IsBranchNotFound returns true when a 404 references a branch path.
func IsBranchNotFound(err error) bool {
	var e *Error
	return err != nil && errors.As(err, &e) && e.StatusCode == http.StatusNotFound
}

// IsNotFound returns true for any 404.
func IsNotFound(err error) bool {
	var e *Error
	return err != nil && errors.As(err, &e) && e.StatusCode == http.StatusNotFound
}

// IsConflict returns true for 409 conflicts.
func IsConflict(err error) bool {
	var e *Error
	return err != nil && errors.As(err, &e) && e.StatusCode == http.StatusConflict
}

// IsAlreadyExists returns true for any "already exists" response.
func IsAlreadyExists(err error) bool {
	var e *Error
	return err != nil && errors.As(err, &e) && e.IsAlreadyExists()
}

// IsRateLimited returns true for 429 or secondary-rate-limit 403s.
func IsRateLimited(err error) bool {
	var e *Error
	return err != nil && errors.As(err, &e) && e.IsRateLimited()
}
