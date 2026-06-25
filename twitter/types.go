package twitter

import (
	"encoding/json"
	"net/url"
	"strings"
)

// ── Requests ────────────────────────────────────────────────────────────────

// CreateTweetRequest is the body for POST /2/tweets. The spec marks no field
// required; reply-only / quote-only / poll-only / card posts are all valid.
type CreateTweetRequest struct {
	Text                  string      `json:"text,omitempty"`
	Reply                 *Reply      `json:"reply,omitempty"`
	QuoteTweetID          string      `json:"quote_tweet_id,omitempty"`
	Media                 *TweetMedia `json:"media,omitempty"`
	Poll                  *Poll       `json:"poll,omitempty"`
	ReplySettings         string      `json:"reply_settings,omitempty"` // "following" | "mentionedUsers" | "subscribers" | "verified"
	ForSuperFollowersOnly *bool       `json:"for_super_followers_only,omitempty"`
}

// Reply configures a tweet as a reply to another tweet.
type Reply struct {
	InReplyToTweetID          string   `json:"in_reply_to_tweet_id"`
	ExcludeReplyUserIDs       []string `json:"exclude_reply_user_ids,omitempty"`
	AutoPopulateReplyMetadata *bool    `json:"auto_populate_reply_metadata,omitempty"`
}

// TweetMedia attaches uploaded media to a tweet.
type TweetMedia struct {
	MediaIDs      []string `json:"media_ids"` // required when Media is set
	TaggedUserIDs []string `json:"tagged_user_ids,omitempty"`
}

// Poll attaches a poll to a tweet.
type Poll struct {
	Options         []string `json:"options"`
	DurationMinutes int      `json:"duration_minutes"`
	ReplySettings   string   `json:"reply_settings,omitempty"`
}

// initMediaRequest is the JSON body for POST /2/media/upload/initialize.
type initMediaRequest struct {
	MediaType     string `json:"media_type"`
	TotalBytes    int    `json:"total_bytes"` // = len(data); omitting it breaks the session
	MediaCategory string `json:"media_category"`
}

// metadataCreateRequest is the body for POST /2/media/metadata. The shape is
// three levels deep: {id, metadata:{alt_text:{text}}}.
type metadataCreateRequest struct {
	ID       string       `json:"id"`
	Metadata metadataBody `json:"metadata"`
}

type metadataBody struct {
	AltText *altText `json:"alt_text,omitempty"`
}

type altText struct {
	Text string `json:"text"`
}

// ── Read models ───────────────────────────────────────────────────────────────

// User models the X User schema. id/name/username are always present.
type User struct {
	ID              string       `json:"id"`
	Name            string       `json:"name"`
	Username        string       `json:"username"`
	CreatedAt       string       `json:"created_at,omitempty"`
	Description     string       `json:"description,omitempty"`
	ProfileImageURL string       `json:"profile_image_url,omitempty"`
	Verified        bool         `json:"verified,omitempty"`
	Protected       bool         `json:"protected,omitempty"`
	PublicMetrics   *UserMetrics `json:"public_metrics,omitempty"`
	PinnedTweetID   string       `json:"pinned_tweet_id,omitempty"`
}

// UserMetrics field names are verified against the live X API/docs, NOT
// derivable from openapi.json (the spec's UserPublicMetrics schema is empty).
type UserMetrics struct {
	FollowersCount int `json:"followers_count"`
	FollowingCount int `json:"following_count"`
	TweetCount     int `json:"tweet_count"`
	ListedCount    int `json:"listed_count"`
	LikeCount      int `json:"like_count"`
}

// Tweet models the X Tweet schema. On create only id+text are guaranteed; the
// other fields are populated only on reads that request them.
type Tweet struct {
	ID             string        `json:"id"`
	Text           string        `json:"text"`
	AuthorID       string        `json:"author_id,omitempty"`
	CreatedAt      string        `json:"created_at,omitempty"`
	ConversationID string        `json:"conversation_id,omitempty"`
	Lang           string        `json:"lang,omitempty"`
	PublicMetrics  *TweetMetrics `json:"public_metrics,omitempty"`
}

// TweetMetrics field names are verified against the live X API/docs, NOT
// derivable from openapi.json (the spec's TweetPublicMetrics schema is empty).
type TweetMetrics struct {
	RetweetCount    int `json:"retweet_count"`
	ReplyCount      int `json:"reply_count"`
	LikeCount       int `json:"like_count"`
	QuoteCount      int `json:"quote_count"`
	ImpressionCount int `json:"impression_count"`
	BookmarkCount   int `json:"bookmark_count"`
}

// Media is the data payload of a MediaUploadResponse (not the whole body).
type Media struct {
	ID               string          `json:"id"`
	MediaKey         string          `json:"media_key,omitempty"`
	Size             int             `json:"size,omitempty"`
	ExpiresAfterSecs int             `json:"expires_after_secs,omitempty"`
	ProcessingInfo   *ProcessingInfo `json:"processing_info,omitempty"`
}

// ProcessingInfo reports asynchronous media processing state.
type ProcessingInfo struct {
	State           string `json:"state"` // pending | in_progress | succeeded | failed
	CheckAfterSecs  int    `json:"check_after_secs,omitempty"`
	ProgressPercent int    `json:"progress_percent,omitempty"`
}

// deleteResult is the data payload of TweetDeleteResponse.
type deleteResult struct {
	Deleted bool `json:"deleted"`
}

// ── Envelopes ─────────────────────────────────────────────────────────────────

// dataEnvelope is the generic single/array envelope {data, errors, meta}. No
// Tier-1 method surfaces "includes"/expansions, so it is intentionally omitted.
type dataEnvelope[T any] struct {
	Data   T         `json:"data"`
	Errors []Problem `json:"errors,omitempty"`
	Meta   *PageMeta `json:"meta,omitempty"`
}

// PageMeta carries pagination metadata from list responses.
type PageMeta struct {
	ResultCount   int    `json:"result_count,omitempty"`
	NextToken     string `json:"next_token,omitempty"`
	PreviousToken string `json:"previous_token,omitempty"`
	NewestID      string `json:"newest_id,omitempty"`
	OldestID      string `json:"oldest_id,omitempty"`
}

// TweetPage is the result of GetUserTweets. Meta is a pointer (it mirrors the
// envelope's *PageMeta; guard before dereferencing).
type TweetPage struct {
	Tweets []Tweet
	Meta   *PageMeta
}

// TimelineParams maps to /2/users/{id}/tweets request query params. The request
// cursor is "pagination_token"; the response carries meta.next_token. Feed
// TweetPage.Meta.NextToken back into PaginationToken to page forward.
type TimelineParams struct {
	MaxResults      int      // -> max_results
	PaginationToken string   // -> pagination_token (NOT next_token)
	Exclude         []string // -> exclude (comma-joined: "retweets","replies")
	SinceID         string   // -> since_id
	UntilID         string   // -> until_id
	StartTime       string   // -> start_time
	EndTime         string   // -> end_time
	Fields          []FieldOpt
}

// ── Field options ─────────────────────────────────────────────────────────────

// FieldOpt sets a single field-selection query key. Each category maps to
// exactly one comma-joined query value (via url.Values.Set), so repeated opts
// of the same kind replace rather than duplicate the key.
type FieldOpt func(url.Values)

// WithUserFields sets the user.fields query parameter.
func WithUserFields(fields ...string) FieldOpt {
	return func(v url.Values) { v.Set("user.fields", strings.Join(fields, ",")) }
}

// WithTweetFields sets the tweet.fields query parameter.
func WithTweetFields(fields ...string) FieldOpt {
	return func(v url.Values) { v.Set("tweet.fields", strings.Join(fields, ",")) }
}

// WithExpansions sets the expansions query parameter.
func WithExpansions(expansions ...string) FieldOpt {
	return func(v url.Values) { v.Set("expansions", strings.Join(expansions, ",")) }
}

// withIDs sets the ids query parameter to a pre-joined comma-separated value.
func withIDs(joined string) FieldOpt {
	return func(v url.Values) { v.Set("ids", joined) }
}

// applyFieldOpts builds a query string from the given options. The returned
// string includes a leading "?" when non-empty, ready to append to a path.
func applyFieldOpts(opts []FieldOpt) string {
	v := url.Values{}
	for _, opt := range opts {
		opt(v)
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// ── Tier 2: engagement, audience, analytics ─────────────────────────────────

// UserPage mirrors TweetPage for endpoints returning a user list + pagination
// (followers, following, liking users, retweeters, user search). Meta is a
// pointer (guard before dereferencing).
type UserPage struct {
	Users []User
	Meta  *PageMeta
}

// PageParams is the minimal cursor for follower/member-style lists. The request
// cursor is pagination_token; the response carries meta.next_token. Feed
// UserPage.Meta.NextToken back into PaginationToken to page forward.
type PageParams struct {
	MaxResults      int        // -> max_results
	PaginationToken string     // -> pagination_token (NOT next_token)
	Fields          []FieldOpt // user.fields, tweet.fields, expansions
}

// SearchParams maps to /2/tweets/search/recent. NOTE: the request cursor key
// here is "next_token" (NOT pagination_token). Feed TweetPage.Meta.NextToken
// back into NextToken to page forward.
type SearchParams struct {
	MaxResults int        // -> max_results
	NextToken  string     // -> next_token (search uses next_token, not pagination_token)
	StartTime  string     // -> start_time
	EndTime    string     // -> end_time
	SinceID    string     // -> since_id
	UntilID    string     // -> until_id
	SortOrder  string     // -> sort_order ("recency" | "relevancy")
	Fields     []FieldOpt // tweet.fields, expansions, ...
}

// UserSearchParams maps to /2/users/search. The request cursor key is
// "next_token" (NOT pagination_token).
type UserSearchParams struct {
	MaxResults int        // -> max_results
	NextToken  string     // -> next_token
	Fields     []FieldOpt // user.fields, ...
}

// FollowResult is the data payload of UsersFollowingCreateResponse. A protected
// target yields PendingFollow=true with Following=false until accepted.
type FollowResult struct {
	Following     bool `json:"following"`
	PendingFollow bool `json:"pending_follow"`
}

// Engagement write-response data payloads (each is a dataEnvelope[T].Data):
type likeResult struct {
	Liked bool `json:"liked"`
}
type retweetResult struct {
	Retweeted bool `json:"retweeted"`
}
type hideResult struct {
	Hidden bool `json:"hidden"`
}
type unfollowResult struct {
	Following bool `json:"following"`
}

// Request bodies (single-field; typed for clarity):
type tweetIDBody struct {
	TweetID string `json:"tweet_id"`
}
type targetIDBody struct {
	TargetUserID string `json:"target_user_id"`
}
type hiddenBody struct {
	Hidden bool `json:"hidden"`
}

// TweetAnalytics is one tweet's analytics row from GET /2/tweets/analytics.
type TweetAnalytics struct {
	ID                 string              `json:"id"`
	TimestampedMetrics []TimestampedMetric `json:"timestamped_metrics"`
}

// MediaAnalytics is one media object's analytics row from GET /2/media/analytics.
type MediaAnalytics struct {
	MediaKey           string              `json:"media_key"`
	TimestampedMetrics []TimestampedMetric `json:"timestamped_metrics"`
}

// TimestampedMetric is a single time-bucketed metrics snapshot. The spec
// enumerates a fixed set of integer metric fields (19 for tweets, 10 for media)
// whose availability varies by API access tier, so Metrics is modelled as a
// map rather than coupling to a per-tier field list (all values are integers).
type TimestampedMetric struct {
	Timestamp string           `json:"timestamp"`
	Metrics   map[string]int64 `json:"metrics"`
}

// AnalyticsParams covers GET /2/tweets/analytics. All four fields are REQUIRED
// by the spec and validated client-side (presence-only) before the call.
// Granularity is validated as non-empty only, never against the value set,
// matching the package's no-enum-value-validation convention.
type AnalyticsParams struct {
	IDs         []string // -> ids (comma-joined)   required
	StartTime   string   // -> start_time           required
	EndTime     string   // -> end_time             required
	Granularity string   // "hourly" | "daily" | "weekly" | "total"  required
}

// MediaAnalyticsParams covers GET /2/media/analytics. All four fields are
// REQUIRED. NOTE the granularity enum DIFFERS from AnalyticsParams: media
// analytics does not allow "weekly".
type MediaAnalyticsParams struct {
	MediaKeys   []string // -> media_keys (comma-joined)  required
	StartTime   string   // -> start_time                 required
	EndTime     string   // -> end_time                   required
	Granularity string   // "hourly" | "daily" | "total" (no weekly)  required
}

// ── Tier 3: lists, bookmarks, trends, articles ───────────────────────────────
//
// The page-cursor primitives PageParams, UserPage, and pageQuery (defined with
// Tier 2 above) are reused here; list/member/follower/bookmark reads cursor on
// pagination_token via pageQuery, and tweet collections via timelineQuery.

// List models the X List schema (id+name always present).
type List struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	Private       bool   `json:"private"` // NOT omitempty: keep an explicit false on re-marshal
	OwnerID       string `json:"owner_id,omitempty"`
	FollowerCount int    `json:"follower_count,omitempty"`
	MemberCount   int    `json:"member_count,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
}

// ListPage mirrors TweetPage/UserPage for paginated list collections
// (owned_lists, list_memberships, followed_lists). Meta is a pointer (guard
// before dereferencing).
type ListPage struct {
	Lists []List
	Meta  *PageMeta
}

// Trend is one item of the personalized_trends response (schema
// PersonalizedTrend). No pagination.
type Trend struct {
	TrendName     string `json:"trend_name"`
	Category      string `json:"category,omitempty"`
	PostCount     int    `json:"post_count,omitempty"`
	TrendingSince string `json:"trending_since,omitempty"`
}

// Article is the data payload of the article *draft* response ({id, title}).
// The *publish* response is {post_id} only and is NOT decoded into this type.
type Article struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

// CreateListRequest is the body for POST /2/lists. Only name is required.
type CreateListRequest struct {
	Name        string `json:"name"` // required
	Description string `json:"description,omitempty"`
	Private     *bool  `json:"private,omitempty"`
}

// UpdateListRequest is the body for PUT /2/lists/{id}. No field is
// server-required; an empty body is valid and sent as {}.
type UpdateListRequest struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	Private     *bool   `json:"private,omitempty"`
}

// ArticleDraftRequest is the body for POST /2/articles/draft. title +
// content_state are required by the spec; content_state and cover_media are
// opaque objects, so they are carried as raw JSON (encoding/json is stdlib —
// go.mod stays unchanged).
type ArticleDraftRequest struct {
	Title        string          `json:"title"`                 // required
	ContentState json.RawMessage `json:"content_state"`         // required, opaque
	CoverMedia   json.RawMessage `json:"cover_media,omitempty"` // optional, opaque
}

// Request bodies (single-field; typed for clarity). tweetIDBody (AddBookmark)
// is shared with Tier 2 and defined above.
type userIDBody struct {
	UserID string `json:"user_id"`
}
type listIDBody struct {
	ListID string `json:"list_id"`
}

// Write-response data payloads (each is a dataEnvelope[T].Data). DeleteList
// reuses Tier 1's deleteResult{Deleted bool}; CreateList decodes
// dataEnvelope[List] directly — neither needs a dedicated struct here.
type updatedResult struct {
	Updated bool `json:"updated"`
}
type memberResult struct {
	IsMember bool `json:"is_member"`
}
type followResult struct {
	Following bool `json:"following"`
}
type pinnedResult struct {
	Pinned bool `json:"pinned"`
}
type bookmarkResult struct {
	Bookmarked bool `json:"bookmarked"`
}
