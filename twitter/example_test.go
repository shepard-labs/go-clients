package twitter_test

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.uber.org/zap"

	"github.com/shepard-labs/go-clients/twitter"
)

// newClient builds a client from an OAuth2 user-context access token. The token
// is the one a user grants when they link their X account; refreshing it is the
// caller's responsibility.
func newClient() *twitter.Client {
	logger, _ := zap.NewProduction()
	return twitter.New(os.Getenv("X_ACCESS_TOKEN"), logger)
}

// Example shows the end-to-end "upload media, then post with it" flow that most
// callers want.
func Example() {
	ctx := context.Background()
	c := newClient()

	img, err := os.ReadFile("launch.jpg")
	if err != nil {
		return
	}

	media, err := c.UploadMedia(ctx, img, "image/jpeg", "tweet_image")
	if err != nil {
		return
	}

	// Optional: attach alt text for accessibility.
	_ = c.SetMediaMetadata(ctx, media.ID, "A rocket lifting off at dawn")

	tweet, err := c.CreateTweet(ctx, &twitter.CreateTweetRequest{
		Text:  "Launch day 🚀",
		Media: &twitter.TweetMedia{MediaIDs: []string{media.ID}},
	})
	if err != nil {
		return
	}
	fmt.Println(tweet.ID)
}

// ── Publishing ────────────────────────────────────────────────────────────────

// ExampleClient_CreateTweet posts a plain-text tweet.
func ExampleClient_CreateTweet() {
	ctx := context.Background()
	c := newClient()

	tweet, err := c.CreateTweet(ctx, &twitter.CreateTweetRequest{
		Text: "Hello from go-clients",
	})
	if err != nil {
		return
	}
	fmt.Println(tweet.ID, tweet.Text)
}

// ExampleClient_CreateTweet_reply posts a reply to an existing tweet. A
// reply-only request (no Text) is valid.
func ExampleClient_CreateTweet_reply() {
	ctx := context.Background()
	c := newClient()

	_, _ = c.CreateTweet(ctx, &twitter.CreateTweetRequest{
		Text:  "Couldn't agree more.",
		Reply: &twitter.Reply{InReplyToTweetID: "1455953449422516226"},
	})
}

// ExampleClient_CreateTweet_quote quotes another tweet.
func ExampleClient_CreateTweet_quote() {
	ctx := context.Background()
	c := newClient()

	_, _ = c.CreateTweet(ctx, &twitter.CreateTweetRequest{
		Text:         "This is worth a read:",
		QuoteTweetID: "1455953449422516226",
	})
}

// ExampleClient_CreateTweet_poll attaches a poll.
func ExampleClient_CreateTweet_poll() {
	ctx := context.Background()
	c := newClient()

	_, _ = c.CreateTweet(ctx, &twitter.CreateTweetRequest{
		Text: "Best Go web framework?",
		Poll: &twitter.Poll{
			Options:         []string{"net/http", "gin", "echo", "chi"},
			DurationMinutes: 1440,
		},
	})
}

// ExampleClient_CreateTweet_restrictReplies limits who can reply.
func ExampleClient_CreateTweet_restrictReplies() {
	ctx := context.Background()
	c := newClient()

	_, _ = c.CreateTweet(ctx, &twitter.CreateTweetRequest{
		Text:          "Announcement for followers.",
		ReplySettings: "following", // "following" | "mentionedUsers" | "subscribers" | "verified"
	})
}

// ExampleClient_DeleteTweet deletes a tweet and reports whether it was deleted.
func ExampleClient_DeleteTweet() {
	ctx := context.Background()
	c := newClient()

	deleted, err := c.DeleteTweet(ctx, "1455953449422516226")
	if err != nil {
		return
	}
	fmt.Println(deleted)
}

// ── Profile & account reads ─────────────────────────────────────────────────

// ExampleClient_Me reads the authenticated user's profile with its public
// metrics. Field options are requested via WithUserFields.
func ExampleClient_Me() {
	ctx := context.Background()
	c := newClient()

	me, err := c.Me(ctx, twitter.WithUserFields("public_metrics", "description"))
	if err != nil {
		return
	}
	if me.PublicMetrics != nil {
		fmt.Printf("@%s has %d followers\n", me.Username, me.PublicMetrics.FollowersCount)
	}
}

// ExampleClient_GetUser fetches a single user by id.
func ExampleClient_GetUser() {
	ctx := context.Background()
	c := newClient()

	u, err := c.GetUser(ctx, "2244994945", twitter.WithUserFields("profile_image_url", "verified"))
	if err != nil {
		return
	}
	fmt.Println(u.Name, u.Verified)
}

// ExampleClient_GetUserByUsername fetches a single user by @username.
func ExampleClient_GetUserByUsername() {
	ctx := context.Background()
	c := newClient()

	u, err := c.GetUserByUsername(ctx, "XDevelopers")
	if err != nil {
		return
	}
	fmt.Println(u.ID)
}

// ExampleClient_GetUsers fetches several users in one call; the ids are sent as
// a single comma-joined value.
func ExampleClient_GetUsers() {
	ctx := context.Background()
	c := newClient()

	users, err := c.GetUsers(ctx, []string{"2244994945", "783214"}, twitter.WithUserFields("public_metrics"))
	if err != nil {
		return
	}
	for _, u := range users {
		fmt.Println(u.Username)
	}
}

// ExampleClient_GetUserTweets pages through a user's timeline. The request
// cursor is pagination_token; feed TweetPage.Meta.NextToken back in to advance.
func ExampleClient_GetUserTweets() {
	ctx := context.Background()
	c := newClient()

	params := &twitter.TimelineParams{
		MaxResults: 100,
		Exclude:    []string{"retweets", "replies"},
		Fields:     []twitter.FieldOpt{twitter.WithTweetFields("created_at", "public_metrics")},
	}

	for {
		page, err := c.GetUserTweets(ctx, "2244994945", params)
		if err != nil {
			return
		}
		for _, t := range page.Tweets {
			fmt.Println(t.ID, t.Text)
		}
		if page.Meta == nil || page.Meta.NextToken == "" {
			break
		}
		params.PaginationToken = page.Meta.NextToken
	}
}

// ── Tweet reads ───────────────────────────────────────────────────────────────

// ExampleClient_GetTweet fetches a single tweet with its public metrics.
func ExampleClient_GetTweet() {
	ctx := context.Background()
	c := newClient()

	t, err := c.GetTweet(ctx, "1455953449422516226", twitter.WithTweetFields("public_metrics", "created_at"))
	if err != nil {
		return
	}
	if t.PublicMetrics != nil {
		fmt.Printf("%d likes, %d reposts\n", t.PublicMetrics.LikeCount, t.PublicMetrics.RetweetCount)
	}
}

// ExampleClient_GetTweets fetches several tweets in one call.
func ExampleClient_GetTweets() {
	ctx := context.Background()
	c := newClient()

	tweets, err := c.GetTweets(ctx, []string{"1455953449422516226", "1460323737035677698"})
	if err != nil {
		return
	}
	fmt.Println(len(tweets))
}

// ── Media ─────────────────────────────────────────────────────────────────────

// ExampleClient_UploadMedia uploads an image. UploadMedia runs the full chunked
// flow (initialize → append → finalize → poll) and returns once processing is
// complete. category must be one of tweet_image, tweet_video, tweet_gif; note
// image/jpg is invalid — use image/jpeg.
func ExampleClient_UploadMedia() {
	ctx := context.Background()
	c := newClient()

	data, err := os.ReadFile("clip.mp4")
	if err != nil {
		return
	}

	media, err := c.UploadMedia(ctx, data, "video/mp4", "tweet_video")
	if err != nil {
		return
	}
	fmt.Println(media.ID)
}

// ExampleClient_SetMediaMetadata attaches alt text to uploaded media for
// accessibility, before referencing it from a tweet.
func ExampleClient_SetMediaMetadata() {
	ctx := context.Background()
	c := newClient()

	if err := c.SetMediaMetadata(ctx, "1146654567674912769", "A golden retriever puppy"); err != nil {
		return
	}
}

// ── Error handling ──────────────────────────────────────────────────────────

// ExampleAPIError shows how to branch on the typed error: a 401 means the token
// expired and the caller should refresh it and rebuild the client; a 403 means
// a missing OAuth2 scope; a 429 means rate-limited.
func ExampleAPIError() {
	ctx := context.Background()
	c := newClient()

	_, err := c.Me(ctx)

	var apiErr *twitter.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.IsUnauthorized():
		fmt.Println("token expired — refresh OAuth2 token and rebuild client")
	case errors.As(err, &apiErr) && apiErr.IsForbidden():
		fmt.Println("missing scope or insufficient access tier")
	case errors.As(err, &apiErr) && apiErr.IsRateLimit():
		fmt.Println("rate limited — back off and retry later")
	case errors.Is(err, twitter.ErrInvalidRequest):
		fmt.Println("client-side validation failed")
	case err != nil:
		fmt.Println("request failed:", err)
	}
}
