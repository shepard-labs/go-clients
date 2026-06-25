# go-clients

[![CI](https://github.com/shepard-labs/go-clients/actions/workflows/ci.yml/badge.svg)](https://github.com/shepard-labs/go-clients/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/shepard-labs/go-clients)](https://goreportcard.com/report/github.com/shepard-labs/go-clients)
[![Go Reference](https://pkg.go.dev/badge/github.com/shepard-labs/go-clients.svg)](https://pkg.go.dev/github.com/shepard-labs/go-clients)

Reusable Go clients for cloud services, extracted as independent modules so a
backend can import only what it needs.

Multi-module monorepo: each top-level directory is its own Go module with its
own dependency set. A consumer that needs only KMS pulls no email or storage
SDKs into its build graph.

## Modules

| Module | Import path | Providers |
|---|---|---|
| `email` | `github.com/shepard-labs/go-clients/email` | SES (`/ses`), Postmark (`/postmark`) |
| `storage` | `github.com/shepard-labs/go-clients/storage` | GCS (`/gcs`), Cloudflare R2 (`/r2`) |
| `kms` | `github.com/shepard-labs/go-clients/kms` | Google Cloud KMS |
| `twitter` | `github.com/shepard-labs/go-clients/twitter` | X (Twitter) API v2 |

Each module exposes a single capability interface; the provider subpackages
implement it and bind their credentials/configuration at construction.

## email

```go
import (
    "github.com/shepard-labs/go-clients/email"
    "github.com/shepard-labs/go-clients/email/ses"
    "github.com/shepard-labs/go-clients/email/postmark"
)

// Pick a provider; both return email.Sender.
var sender email.Sender
sender = ses.New(ses.Credentials{AccessKeyID: id, SecretAccessKey: secret, Region: "us-east-1"}, "my-service", logger)
sender = postmark.New(serverToken, logger)

res, err := sender.Send(ctx, &email.Message{
    From:     "no-reply@example.com",
    To:       []string{"user@example.com"},
    Subject:  "Hello",
    HTMLBody: "<p>Hi</p>",
})
err = sender.VerifyAuth(ctx)
```

The `Sender` interface covers `Send` and `VerifyAuth`. Provider-specific
operations (SES identity management, Postmark domain verification, account /
server introspection) are methods on the concrete `*ses.Client` /
`*postmark.Client` and reachable via a type assertion.

## storage

```go
import (
    "github.com/shepard-labs/go-clients/storage"
    "github.com/shepard-labs/go-clients/storage/gcs"
    "github.com/shepard-labs/go-clients/storage/r2"
)

var s storage.Storage
s, err = gcs.New(ctx, serviceAccountB64, "my-bucket", "my-service", logger)
s, err = r2.New(accountID, accessKey, secret, "my-bucket", "my-service", logger)

err = s.Upload(ctx, "path/to/object", content, "application/pdf")
err = s.UploadReader(ctx, "path/to/object", reader, "application/pdf", size)
data, err := s.Download(ctx, "path/to/object")
err = s.Delete(ctx, "path/to/object")
err = s.Close()
```

The bucket is supplied at construction. Application-specific path conventions
are intentionally out of scope — build them on top of a `storage.Storage`
value.

## kms

```go
import "github.com/shepard-labs/go-clients/kms"

enc, err := kms.New(ctx, serviceAccountB64,
    "projects/p/locations/global/keyRings/r/cryptoKeys/k", logger)

ciphertext, err := enc.EncryptCredentials(ctx, plaintext)
plaintext, err := enc.DecryptCredentials(ctx, ciphertext)
err = enc.Close()
```

The full KMS crypto-key resource name is supplied at construction.

## twitter

A hand-written client for a curated slice of the X (Twitter) API v2: publishing
posts, chunked media upload, reading back an account's own content and profile
, plus engagement, audience monitoring, follower management, and
analytics, plus list curation, bookmarks, personalized trends, and
long-form articles. It is built for a tool where users link their X
account(s) via OAuth2.

```go
import "github.com/shepard-labs/go-clients/twitter"

c := twitter.New(userAccessToken, logger) // OAuth2 user-context token

me, err := c.Me(ctx, twitter.WithUserFields("public_metrics"))
m, err := c.UploadMedia(ctx, imgBytes, "image/jpeg", "tweet_image")
t, err := c.CreateTweet(ctx, &twitter.CreateTweetRequest{
    Text:  "Launch day 🚀",
    Media: &twitter.TweetMedia{MediaIDs: []string{m.ID}},
})
```

### Engagement & audience

Engagement actions, audience monitoring, follower management, and analytics on
the linked account — all additive methods on the same `*Client`:

```go
// Engagement (boolean writers).
liked, err := c.Like(ctx, myUserID, "1455953449422516226")
_, err = c.Retweet(ctx, myUserID, "1455953449422516226")

// Search the last 7 days. The request cursor is next_token (not
// pagination_token); feed page.Meta.NextToken back into SearchParams.NextToken.
page, err := c.SearchRecent(ctx, "from:XDevelopers -is:retweet", &twitter.SearchParams{
    MaxResults: 100,
    Fields:     []twitter.FieldOpt{twitter.WithTweetFields("public_metrics", "created_at")},
})

// Followers page on pagination_token.
followers, err := c.GetFollowers(ctx, myUserID, &twitter.PageParams{MaxResults: 1000})

// Analytics requires an elevated X API access tier — a 403
// (APIError.IsForbidden()) means the linked account/app lacks the tier.
rows, err := c.GetTweetAnalytics(ctx, &twitter.AnalyticsParams{
    IDs:         []string{"1455953449422516226"},
    StartTime:   "2026-01-01T00:00:00Z",
    EndTime:     "2026-01-08T00:00:00Z",
    Granularity: "daily", // "hourly" | "daily" | "weekly" | "total"
})
```

### Lists, bookmarks & trends

List curation (audience segments and monitoring groups), bookmark management,
and personalized trends for content ideation — all additive methods on the same
`*Client`:

```go
// Create a List and segment an audience into it.
private := true
list, err := c.CreateList(ctx, &twitter.CreateListRequest{
    Name:        "Prospects",
    Description: "Accounts to monitor",
    Private:     &private, // optional; omitted when nil
})
_, err = c.AddListMember(ctx, list.ID, "783214") // returns is_member

// Read a List's members and tweets. Member/follower reads page on
// pagination_token; feed page.Meta.NextToken back into PageParams.PaginationToken.
members, err := c.GetListMembers(ctx, list.ID, &twitter.PageParams{MaxResults: 100})
tweets, err := c.GetListTweets(ctx, list.ID, &twitter.TimelineParams{MaxResults: 50})

// List memberships from the user side.
followed, err := c.GetFollowedLists(ctx, myUserID, nil) // *ListPage
pinned, err := c.GetPinnedLists(ctx, myUserID)           // []List (no pagination)
_, err = c.FollowList(ctx, myUserID, list.ID)            // returns following

// Bookmarks: curate saved content. GetBookmarks pages on pagination_token.
saved, err := c.GetBookmarks(ctx, myUserID, &twitter.TimelineParams{MaxResults: 100})
_, err = c.AddBookmark(ctx, myUserID, "1455953449422516226") // returns bookmarked

// Personalized trends for content ideation (no parameters).
trends, err := c.GetPersonalizedTrends(ctx)

// Articles require an eligible X API access tier — a 403
// (APIError.IsForbidden()) means the linked account/app lacks the access.
draft, err := c.CreateArticleDraft(ctx, &twitter.ArticleDraftRequest{
    Title:        "Launch retrospective",
    ContentState: contentStateJSON, // opaque editor state (json.RawMessage)
})
postID, err := c.PublishArticle(ctx, draft.ID) // returns the new post id
```


The access token is bound at construction. On a `401`
(`twitter.APIError.IsUnauthorized()`) the caller refreshes the OAuth2 token and
builds a new client — token acquisition and refresh are caller-owned. `*Client`
is safe for concurrent use.

Required OAuth2 scopes (a `403` / `APIError.IsForbidden()` usually means a
missing scope or insufficient access tier):

| Operation | Scopes |
|---|---|
| `CreateTweet`, `DeleteTweet` | `tweet.write` (+ `tweet.read`, `users.read`) |
| User / tweet reads | `users.read`, `tweet.read` |
| `UploadMedia`, `SetMediaMetadata` | `media.write` |
| `Like`, `Unlike` | `like.write` (+ `tweet.read`, `users.read`) |
| `Retweet`, `Unretweet` | `tweet.write` (+ `tweet.read`, `users.read`) |
| `Follow`, `Unfollow` | `follows.write` (+ `tweet.read`, `users.read`) |
| `HideReply` | `tweet.moderate.write` (+ `tweet.read`, `users.read`) |
| List reads (`GetList`, `GetListMembers`, …) | `list.read` (+ `tweet.read`, `users.read`) |
| List writes (`CreateList`, `AddListMember`, `FollowList`, `PinList`, …) | `list.write` (+ `list.read`) |
| `GetBookmarks` | `bookmark.read` (+ `tweet.read`, `users.read`) |
| `AddBookmark`, `RemoveBookmark` | `bookmark.write` (+ `bookmark.read`) |
| `GetPersonalizedTrends` | `users.read`, `tweet.read` |
| `CreateArticleDraft`, `PublishArticle` | eligible/elevated access tier (`tweet.write`) |

Bookmark **folders** (a distinct schema), communities, spaces, and DMs remain
future additions on the same `*Client`.
