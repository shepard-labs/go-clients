package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
)

func testPrivateKeyPEM(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b}))
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func TestNewAppClientParsesKeyAndUsesDefaultURL(t *testing.T) {
	_, pemKey := testPrivateKeyPEM(t)
	c, err := NewAppClient("", 42, "my-app", pemKey, zap.NewNop())
	if err != nil {
		t.Fatalf("NewAppClient: %v", err)
	}
	if c.baseURL != DefaultBaseURL || c.appID != 42 || c.appSlug != "my-app" {
		t.Fatalf("unexpected client configuration: %#v", c)
	}
	if _, err := NewAppClient("", 1, "", "not a key", nil); err == nil || !strings.Contains(err.Error(), "failed to parse GitHub App private key") {
		t.Fatalf("invalid key error = %v", err)
	}
}

func TestAppClientAuthAndRepresentativeOperations(t *testing.T) {
	privateKey, pemKey := testPrivateKeyPEM(t)
	var appJWTSeen, installationTokenSeen bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/7/access_tokens":
			appJWTSeen = validateAppJWT(t, r, privateKey)
			if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
				t.Fatalf("unexpected token request: %s %s headers=%v", r.Method, r.URL, r.Header)
			}
			writeJSON(t, w, http.StatusCreated, InstallationAccessToken{Token: "installation-token", ExpiresAt: time.Now().Add(time.Hour)})
		case r.URL.Path == "/app/installations/7":
			if !validateAppJWT(t, r, privateKey) {
				t.Error("installation request did not carry a valid app JWT")
			}
			writeJSON(t, w, http.StatusOK, Installation{ID: 7, Account: Account{Login: "acme"}})
		default:
			installationTokenSeen = r.Header.Get("Authorization") == "Bearer installation-token"
			if !installationTokenSeen || r.Header.Get("Accept") != "application/vnd.github+json" {
				t.Errorf("unexpected installation auth headers: %v", r.Header)
			}
			switch {
			case r.URL.Path == "/installation/repositories":
				writeJSON(t, w, http.StatusOK, map[string]any{"repositories": []Repository{{ID: 9, Name: "repo", DefaultBranch: "main"}}})
			case r.URL.Path == "/repositories/9/branches":
				w.Header().Set("Link", `<http://example.test/repositories/9/branches?after=next>; rel="next"`)
				writeJSON(t, w, http.StatusOK, []Branch{{Name: "main"}})
			case r.URL.Path == "/repositories/9/contents/docs/readme.md":
				writeJSON(t, w, http.StatusOK, ContentFile{Name: "readme.md", Path: "docs/readme.md", SHA: "file-sha"})
			case r.URL.Path == "/repositories/9/pulls" && r.Method == http.MethodPost:
				var payload CreatePullRequest
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Head != "feature" {
					t.Errorf("PR payload = %#v, err=%v", payload, err)
				}
				writeJSON(t, w, http.StatusCreated, PullRequest{Number: 11, Title: payload.Title})
			case r.URL.Path == "/repositories/9/pulls/11" && r.Method == http.MethodPatch:
				writeJSON(t, w, http.StatusOK, PullRequest{Number: 11, Title: "updated"})
			default:
				t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				writeJSON(t, w, http.StatusNotFound, map[string]string{"message": "not found"})
			}
		}
	}))
	defer server.Close()

	c, err := NewAppClient(server.URL+"/", 123, "app", pemKey, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	installation, err := c.Installation(context.Background(), 7)
	if err != nil || installation.Account.Login != "acme" {
		t.Fatalf("Installation = %#v, err=%v", installation, err)
	}
	repos, err := c.InstallationRepositories(context.Background(), 7, 1, 0)
	if err != nil || len(repos) != 1 || repos[0].ID != 9 {
		t.Fatalf("repositories = %#v, err=%v", repos, err)
	}
	branches, cursor, err := c.ListBranches(context.Background(), 7, 9, "old", 10)
	if err != nil || len(branches) != 1 || cursor != "next" {
		t.Fatalf("branches=%#v cursor=%q err=%v", branches, cursor, err)
	}
	file, err := c.GetFile(context.Background(), 7, 9, "docs/readme.md", "main")
	if err != nil || file.SHA != "file-sha" {
		t.Fatalf("file=%#v err=%v", file, err)
	}
	pr, err := c.CreatePR(context.Background(), 7, 9, CreatePullRequest{Title: "new", Head: "feature", Base: "main"})
	if err != nil || pr.Number != 11 {
		t.Fatalf("create PR=%#v err=%v", pr, err)
	}
	updated, err := c.UpdatePR(context.Background(), 7, 9, 11, UpdatePullRequest{Title: "updated"})
	if err != nil || updated.Title != "updated" {
		t.Fatalf("update PR=%#v err=%v", updated, err)
	}
	if !appJWTSeen || !installationTokenSeen {
		t.Fatal("expected both app JWT and installation token authentication")
	}
}

func validateAppJWT(t *testing.T, r *http.Request, key *rsa.PrivateKey) bool {
	t.Helper()
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	token, err := jwt.Parse(value, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodRS256 {
			return nil, errors.New("unexpected signing method")
		}
		return &key.PublicKey, nil
	})
	if err != nil || !token.Valid {
		return false
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	return ok && claims["iss"] == float64(123) && claims["aud"] == "github-app"
}

func TestFileWriteAndBranchCreation(t *testing.T) {
	_, pemKey := testPrivateKeyPEM(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/1/access_tokens" {
			writeJSON(t, w, http.StatusCreated, map[string]string{"token": "tok"})
			return
		}
		if r.URL.Path == "/repositories/2/contents/file.txt" {
			var payload CreateOrUpdateFileRequest
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload.Content != "aGk=" || payload.SHA != "old" {
				t.Errorf("file payload=%#v", payload)
			}
			writeJSON(t, w, http.StatusOK, CreateOrUpdateFileResponse{})
			return
		}
		if r.URL.Path == "/repositories/2/git/refs" {
			writeJSON(t, w, http.StatusCreated, map[string]any{"ref": "refs/heads/feature", "object": map[string]string{"sha": "new"}})
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL)
	}))
	defer server.Close()
	c, err := NewAppClient(server.URL, 1, "app", pemKey, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutFile(context.Background(), 1, 2, "file.txt", "main", "update", []byte("hi"), "old"); err != nil {
		t.Fatal(err)
	}
	branch, err := c.CreateBranch(context.Background(), 1, 2, "feature", "base")
	if err != nil {
		t.Fatal(err)
	}
	if branch.Name != "feature" || branch.Commit.SHA != "new" {
		t.Fatalf("branch=%#v", branch)
	}
}

func TestErrorClassificationAndResponseMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-GitHub-Request-Id", "req-1")
		w.Header().Set("Retry-After", "5")
		w.Header().Set("X-RateLimit-Reset", "1700000000")
		writeJSON(t, w, http.StatusForbidden, map[string]string{"message": "You have exceeded a secondary rate limit; already exists"})
	}))
	defer server.Close()
	_, pemKey := testPrivateKeyPEM(t)
	c, err := NewAppClient(server.URL, 1, "app", pemKey, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Installation(context.Background(), 1)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.RequestID != "req-1" || !IsRateLimited(err) || !apiErr.IsTransient() || apiErr.IsPermanent() || !IsAlreadyExists(err) {
		t.Fatalf("unexpected classified error: %#v, %v", apiErr, err)
	}
	if apiErr.RetryAfter.IsZero() || apiErr.RateLimitReset.IsZero() {
		t.Fatalf("missing rate limit metadata: %#v", apiErr)
	}
	if !IsNotFound(&Error{StatusCode: http.StatusNotFound}) || !IsBranchNotFound(&Error{StatusCode: http.StatusNotFound}) || !IsConflict(&Error{StatusCode: http.StatusConflict}) {
		t.Fatal("status helper classification failed")
	}
	if !(&Error{StatusCode: http.StatusForbidden, Message: "resource not accessible"}).IsPermissionDenied() {
		t.Fatal("permission classification failed")
	}
}
