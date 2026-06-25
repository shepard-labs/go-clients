# twitter

A hand-written Go client for a curated slice of the [X (Twitter) API v2](https://docs.x.com/x-api):
publishing posts, chunked media upload, and reading back an account's own
content and profile.

It is built for a tool where users link their X account(s) via OAuth2: you
construct a `*Client` with the user-context access token they granted, then
drive that account. Token acquisition and refresh are the caller's
responsibility.

```go
import "github.com/shepard-labs/go-clients/twitter"
```

```bash
go get github.com/shepard-labs/go-clients/twitter
```

## Features

| Method | Description | Endpoint | Scope |
|---|---|---|---|
| `CreateTweet` | Publish a post: text, reply, quote, poll, and/or attached media | `POST /2/tweets` | `tweet.write` |
| `DeleteTweet` | Delete a post by id | `DELETE /2/tweets/{id}` | `tweet.write` |
| `UploadMedia` | Chunked media upload (init → append → finalize → poll) for images, video, GIFs | `POST /2/media/upload/*` | `media.write` |
| `SetMediaMetadata` | Attach alt text to uploaded media for accessibility | `POST /2/media/metadata` | `media.write` |
| `Me` | Read the authenticated user's profile | `GET /2/users/me` | `users.read` |
| `GetUser` | Read a user by id | `GET /2/users/{id}` | `users.read` |
| `GetUserByUsername` | Read a user by @username | `GET /2/users/by/username/{username}` | `users.read` |
| `GetUsers` | Read up to many users by id in one call | `GET /2/users?ids=` | `users.read` |
| `GetUserTweets` | Page through a user's timeline | `GET /2/users/{id}/tweets` | `tweet.read`, `users.read` |
| `GetTweet` | Read a single tweet by id | `GET /2/tweets/{id}` | `tweet.read` |
| `GetTweets` | Read up to many tweets by id in one call | `GET /2/tweets?ids=` | `tweet.read` |

All field-selection (`tweet.fields`, `user.fields`, `expansions`) is opt-in via
`WithTweetFields` / `WithUserFields` / `WithExpansions`. See [Usage](#usage) for
runnable snippets and [Required OAuth2 scopes](#required-oauth2-scopes) for the
full scope matrix.

## Design

- **One concrete `*Client`.** X is a single provider, so there is no
  cross-provider interface — the package exports `*Client` directly. Later tiers
  (engagement, analytics, lists, bookmarks) drop in as new methods on the same
  client.
- **Immutable and concurrency-safe.** A `*Client` holds no per-call mutable
  state after `New`; share one across goroutines.
- **Bounded retries.** Each request makes up to 3 attempts: transport errors and
  `5xx` responses are retried with a short backoff; `4xx` (including `429`) is
  returned immediately as a typed `*APIError`. The request body is owned as
  bytes and re-sent in full on every attempt, so retries are correct for JSON
  and chunked multipart uploads alike.
- **Plain `net/http`.** The only direct dependency is `go.uber.org/zap` for
  logging.

## Authentication

Pass a user-context OAuth2 access token to `New`. The token is bound for the
lifetime of the client.

```go
logger, _ := zap.NewProduction()
c := twitter.New(userAccessToken, logger)
```

On a `401`, the token is invalid or expired: refresh the OAuth2 token and build
a new client (see [Error handling](#error-handling)).

### Required OAuth2 scopes

A `403` (`APIError.IsForbidden()`) usually means a missing scope or an
insufficient access tier.

| Operation | Scopes |
|---|---|
| `CreateTweet`, `DeleteTweet` | `tweet.write` (+ `tweet.read`, `users.read`) |
| User / tweet reads | `users.read`, `tweet.read` |
| `UploadMedia`, `SetMediaMetadata` | `media.write` |

## Usage

> Runnable, compile-checked versions of every snippet below live in
> [`example_test.go`](./example_test.go) and render on
> [pkg.go.dev](https://pkg.go.dev/github.com/shepard-labs/go-clients/twitter).
> All methods take a `context.Context` as their first argument.

### Publishing

`CreateTweet` accepts text, a reply, a quote, a poll, or attached media — any
combination. Only a fully-empty request is rejected client-side
(`ErrInvalidRequest`); a reply-only, quote-only, or poll-only post is valid.

```go
// Plain text.
tw, err := c.CreateTweet(ctx, &twitter.CreateTweetRequest{Text: "Hello"})

// Reply.
_, err = c.CreateTweet(ctx, &twitter.CreateTweetRequest{
    Text:  "Couldn't agree more.",
    Reply: &twitter.Reply{InReplyToTweetID: "1455953449422516226"},
})

// Quote.
_, err = c.CreateTweet(ctx, &twitter.CreateTweetRequest{
    Text:         "Worth a read:",
    QuoteTweetID: "1455953449422516226",
})

// Poll.
_, err = c.CreateTweet(ctx, &twitter.CreateTweetRequest{
    Text: "Best Go web framework?",
    Poll: &twitter.Poll{Options: []string{"net/http", "gin", "echo"}, DurationMinutes: 1440},
})

// Restrict who can reply: "following" | "mentionedUsers" | "subscribers" | "verified".
_, err = c.CreateTweet(ctx, &twitter.CreateTweetRequest{
    Text:          "Announcement.",
    ReplySettings: "following",
})
```

Delete a tweet; the boolean reports whether it was deleted:

```go
deleted, err := c.DeleteTweet(ctx, "1455953449422516226")
```

### Media

`UploadMedia` is the only entry point you need. It runs the full chunked flow —
initialize, append in ≤4 MB chunks, finalize, then poll processing status — and
returns once the media is ready to attach to a tweet. The lower-level steps are
intentionally unexported so the sequence can't be misused.

`category` is required and must be one of `tweet_image`, `tweet_video`,
`tweet_gif`. `mediaType` must be a valid MIME type — note `image/jpg` is
**invalid**; use `image/jpeg`.

```go
img, _ := os.ReadFile("launch.jpg")
media, err := c.UploadMedia(ctx, img, "image/jpeg", "tweet_image")

// Optional: alt text for accessibility.
err = c.SetMediaMetadata(ctx, media.ID, "A rocket lifting off at dawn")

// Attach to a tweet by media id.
tw, err := c.CreateTweet(ctx, &twitter.CreateTweetRequest{
    Text:  "Launch day 🚀",
    Media: &twitter.TweetMedia{MediaIDs: []string{media.ID}},
})
```

`UploadMedia` blocks while X processes the media (common for video/GIF). The
poll is bounded by the context deadline, so pass a `context.WithTimeout` if you
need an upper bound. On a failure after `initialize`, the returned error names
the `media_id` so a half-finished upload can be diagnosed.

### Profile & account reads

Field selection is requested with the `WithUserFields` / `WithTweetFields` /
`WithExpansions` options. Repeated options of the same kind union into a single
comma-joined query key.

```go
// Authenticated user, with metrics.
me, err := c.Me(ctx, twitter.WithUserFields("public_metrics", "description"))
if me.PublicMetrics != nil {
    fmt.Println(me.PublicMetrics.FollowersCount)
}

// By id, by username, and in bulk (ids are sent as one comma-joined value).
u, err  := c.GetUser(ctx, "2244994945", twitter.WithUserFields("verified"))
u, err   = c.GetUserByUsername(ctx, "XDevelopers")
us, err := c.GetUsers(ctx, []string{"2244994945", "783214"})
```

### Tweet reads

```go
t, err  := c.GetTweet(ctx, "1455953449422516226", twitter.WithTweetFields("public_metrics"))
ts, err := c.GetTweets(ctx, []string{"1455953449422516226", "1460323737035677698"})
```

### Timelines & pagination

`GetUserTweets` returns a `*TweetPage`. The **request** cursor is
`PaginationToken`; the **response** carries `Meta.NextToken`. Feed one into the
other to page forward:

```go
params := &twitter.TimelineParams{
    MaxResults: 100,
    Exclude:    []string{"retweets", "replies"},
    Fields:     []twitter.FieldOpt{twitter.WithTweetFields("created_at", "public_metrics")},
}
for {
    page, err := c.GetUserTweets(ctx, "2244994945", params)
    if err != nil {
        break
    }
    for _, t := range page.Tweets {
        fmt.Println(t.ID, t.Text)
    }
    if page.Meta == nil || page.Meta.NextToken == "" {
        break // Meta is a pointer — guard before dereferencing.
    }
    params.PaginationToken = page.Meta.NextToken
}
```

## Error handling

Non-2xx responses (and 2xx bodies that carry only an `errors[]` array with no
`data`) are returned as `*twitter.APIError`. Use `errors.As` to inspect it and
the predicate methods to branch on the status:

```go
_, err := c.Me(ctx)

var apiErr *twitter.APIError
switch {
case errors.As(err, &apiErr) && apiErr.IsUnauthorized(): // 401
    // Token expired — refresh the OAuth2 token and build a new client.
case errors.As(err, &apiErr) && apiErr.IsForbidden(): // 403
    // Missing scope or insufficient access tier.
case errors.As(err, &apiErr) && apiErr.IsRateLimit(): // 429
    // Rate limited — back off and retry later.
case errors.Is(err, twitter.ErrInvalidRequest):
    // Client-side validation failed before any HTTP request.
}
```

`APIError.StatusCode` is always the HTTP response status. `APIError.Problems`
holds the full RFC-7807 `errors[]` array when present; `Title`, `Detail`, and
`Type` are populated from the response's top-level problem or, failing that, its
first error.

`ErrInvalidRequest` is returned for client-side validation failures (an empty
`CreateTweet`, an empty id slice, a bad media category) before any network call.

## Scope

In scope:
publishing (`CreateTweet`, `DeleteTweet`), media upload (`UploadMedia`,
`SetMediaMetadata`), and account content & profile reads (`Me`, `GetUser`,
`GetUserByUsername`, `GetUsers`, `GetUserTweets`, `GetTweet`, `GetTweets`).

Out of scope: engagement/analytics/audience, Lists, Bookmarks, DMs,
Spaces, Webhooks, app-only Bearer endpoints (firehose, full-archive search,
compliance), expansion `includes` in the envelope, and OAuth2 token
acquisition/refresh (caller-owned). Each is a future additive change on the same
`*Client`.
