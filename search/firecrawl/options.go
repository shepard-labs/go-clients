package firecrawl

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/shepard-labs/go-clients/search"
)

// Optional knobs for POST /v2/scrape, merged over the base {url, formats?}
// payload built from search.ScrapeRequest.
//
// Reachability — options are only reachable through the concrete *Client
// behind search.Client:
//
//	fc, ok := firecrawl.New(apiKey, logger).(*firecrawl.Client)
//	doc, err := fc.ScrapeWithOptions(ctx, req, opts)
//
// A nil *ScrapeOptions sends exactly what Scrape sends today. Every other
// field is omitempty: nil pointers, empty maps/slices, and an all-empty
// Location are omitted from the JSON payload. Scalars are pointers so an
// explicit zero is sendable (notably WaitFor pointing at 0 sends "waitFor"
// as 0 rather than omitting it).
//
// Headers are sent both as the payload "headers" key and as HTTP headers on
// the API request; the HTTP custom headers are applied first and
// Authorization: Bearer is applied after, so the key can never be
// overridden.
//
// Wire reconciliation (read-only truth-check, no vendored deps):
//   - firecrawl npm 4.38.0 (`npm view firecrawl version`).
//   - apps/js-sdk/firecrawl/src/v2/types.ts, ScrapeOptions: onlyMainContent,
//     timeout, waitFor, mobile, headers, includeTags, excludeTags, location
//     ({country, languages} via LocationConfig), skipTlsVerification (note
//     the lowercase "ls" — the Go field is SkipTLSVerification but the wire
//     key is "skipTlsVerification"), removeBase64Images, blockAds, proxy
//     ("basic"|"stealth"|"enhanced"|"auto"; only basic|stealth|auto are
//     accepted here), maxAge, storeInCache, redactPII (boolean |
//     RedactPIIOptions — only the boolean form is modeled here).
//   - apps/api/src/controllers/v2/scrape.ts reads a top-level
//     zeroDataRetention boolean (`req.body.zeroDataRetention ?? false`).
//
// Timeout/WaitFor are server-side milliseconds consumed by Firecrawl; the
// 30s http.Client.Timeout always wins, so a Timeout above ~30s needs a
// longer client injected via WithHTTPClient.

// ScrapeLocation mirrors the upstream LocationConfig wire object
// ({"country", "languages"}).
type ScrapeLocation struct {
	// Country is normalized with Upper(TrimSpace) before validation and
	// dispatch, and must then be 2 ASCII letters (e.g. "US").
	Country string
	// Languages is only sent (and only valid) together with a Country.
	Languages []string
}

// ScrapeOptions holds optional POST /v2/scrape parameters. All fields are
// omitempty; scalar fields are pointers so explicit zeros are sendable.
type ScrapeOptions struct {
	// OnlyMainContent maps to wire key "onlyMainContent".
	OnlyMainContent *bool
	// Timeout maps to wire key "timeout": server-side milliseconds,
	// 1..600000.
	Timeout *int
	// WaitFor maps to wire key "waitFor": server-side milliseconds,
	// 0..30000. A pointer to 0 sends an explicit 0.
	WaitFor *int
	// Mobile maps to wire key "mobile".
	Mobile *bool
	// SkipTLSVerification maps to wire key "skipTlsVerification".
	SkipTLSVerification *bool
	// RemoveBase64Images maps to wire key "removeBase64Images".
	RemoveBase64Images *bool
	// BlockAds maps to wire key "blockAds".
	BlockAds *bool
	// Proxy maps to wire key "proxy": nil omits it; otherwise one of
	// "basic", "stealth", or "auto" (anything else, including a pointer to
	// "", is rejected).
	Proxy *string
	// StoreInCache maps to wire key "storeInCache".
	StoreInCache *bool
	// MaxAge maps to wire key "maxAge": milliseconds, >= 0. Setting it
	// without StoreInCache pointing at true is rejected.
	MaxAge *int
	// Headers maps to wire key "headers". Keys are canonicalized via
	// http.CanonicalHeaderKey and must be non-blank, under 8KB total
	// (sum of len(key)+len(value)), and outside the deny-list
	// (Authorization, Host, Content-Length, Transfer-Encoding, Connection,
	// Trailer, Upgrade).
	Headers map[string]string
	// IncludeTags maps to wire key "includeTags": each entry non-blank and
	// under 128 characters.
	IncludeTags []string
	// ExcludeTags maps to wire key "excludeTags": each entry non-blank and
	// under 128 characters.
	ExcludeTags []string
	// Location maps to wire key "location". A nil Location, or one with an
	// empty Country and no Languages, omits the key. Languages without a
	// Country is rejected.
	Location *ScrapeLocation
	// ZeroDataRetention maps to top-level wire key "zeroDataRetention".
	ZeroDataRetention *bool
	// RedactPII maps to wire key "redactPII" (boolean form only).
	RedactPII *bool
}

// validate reports whether the options are well-formed. A nil receiver is
// valid. It performs no I/O by caller contract; every failure wraps
// search.ErrInvalidRequest. IncludeTags, ExcludeTags, and
// Location.Languages are each capped at 100 entries.
func (o *ScrapeOptions) validate() error {
	if o == nil {
		return nil
	}
	if o.Timeout != nil && (*o.Timeout < 1 || *o.Timeout > 600000) {
		return fmt.Errorf("firecrawl: invalid timeout %d (must be 1..600000 ms): %w", *o.Timeout, search.ErrInvalidRequest)
	}
	if o.WaitFor != nil && (*o.WaitFor < 0 || *o.WaitFor > 30000) {
		return fmt.Errorf("firecrawl: invalid waitFor %d (must be 0..30000 ms): %w", *o.WaitFor, search.ErrInvalidRequest)
	}
	if o.Timeout != nil && o.WaitFor != nil && *o.WaitFor > *o.Timeout {
		return fmt.Errorf("firecrawl: invalid waitFor %d (must be <= timeout %d ms): %w", *o.WaitFor, *o.Timeout, search.ErrInvalidRequest)
	}
	if o.Proxy != nil {
		switch *o.Proxy {
		case "basic", "stealth", "auto":
		default:
			return fmt.Errorf("firecrawl: invalid proxy %q (must be basic, stealth, or auto): %w", *o.Proxy, search.ErrInvalidRequest)
		}
	}
	if o.MaxAge != nil {
		if *o.MaxAge < 0 {
			return fmt.Errorf("firecrawl: invalid maxAge %d (must be >= 0): %w", *o.MaxAge, search.ErrInvalidRequest)
		}
		if o.StoreInCache == nil || !*o.StoreInCache {
			return fmt.Errorf("firecrawl: maxAge requires storeInCache: %w", search.ErrInvalidRequest)
		}
	}
	if len(o.IncludeTags) > maxScrapeListEntries {
		return fmt.Errorf("firecrawl: invalid includeTags (at most %d entries): %w", maxScrapeListEntries, search.ErrInvalidRequest)
	}
	if len(o.ExcludeTags) > maxScrapeListEntries {
		return fmt.Errorf("firecrawl: invalid excludeTags (at most %d entries): %w", maxScrapeListEntries, search.ErrInvalidRequest)
	}
	for i, t := range o.IncludeTags {
		if strings.TrimSpace(t) == "" || len(t) >= 128 {
			return fmt.Errorf("firecrawl: invalid includeTags[%d] (must be non-blank, <128 chars): %w", i, search.ErrInvalidRequest)
		}
	}
	for i, t := range o.ExcludeTags {
		if strings.TrimSpace(t) == "" || len(t) >= 128 {
			return fmt.Errorf("firecrawl: invalid excludeTags[%d] (must be non-blank, <128 chars): %w", i, search.ErrInvalidRequest)
		}
	}
	if err := o.Location.validate(); err != nil {
		return err
	}
	if len(o.Headers) > maxScrapeListEntries {
		return fmt.Errorf("firecrawl: invalid headers (at most %d entries): %w", maxScrapeListEntries, search.ErrInvalidRequest)
	}
	if _, err := o.canonicalHeaders(); err != nil {
		return err
	}
	return nil
}

// validate reports whether the location is well-formed. A nil receiver, or
// one with an empty Country and no Languages (omitted at dispatch), is
// valid. Languages without a Country is rejected; a present Country must
// normalize (Upper(TrimSpace)) to 2 ASCII letters. Languages holds at most
// 100 entries, each non-blank and under 128 characters.
func (l *ScrapeLocation) validate() error {
	if l == nil {
		return nil
	}
	country := strings.ToUpper(strings.TrimSpace(l.Country))
	if country == "" && len(l.Languages) == 0 {
		return nil
	}
	if country == "" {
		return fmt.Errorf("firecrawl: location languages require country: %w", search.ErrInvalidRequest)
	}
	if !validCountryCode(country) {
		return fmt.Errorf("firecrawl: invalid location country %q (must be 2 ASCII letters): %w", l.Country, search.ErrInvalidRequest)
	}
	if len(l.Languages) > maxScrapeListEntries {
		return fmt.Errorf("firecrawl: invalid location languages (at most %d entries): %w", maxScrapeListEntries, search.ErrInvalidRequest)
	}
	for i, lang := range l.Languages {
		if strings.TrimSpace(lang) == "" || len(lang) >= 128 {
			return fmt.Errorf("firecrawl: invalid location languages[%d] (must be non-blank, <128 chars): %w", i, search.ErrInvalidRequest)
		}
	}
	return nil
}

// validCountryCode reports whether s is exactly 2 ASCII uppercase letters.
func validCountryCode(s string) bool {
	if len(s) != 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

// deniedScrapeHeaders are never forwarded: transport-controlled headers or
// the credential header.
var deniedScrapeHeaders = map[string]struct{}{
	"Authorization":     {},
	"Host":              {},
	"Content-Length":    {},
	"Transfer-Encoding": {},
	"Connection":        {},
	"Trailer":           {},
	"Upgrade":           {},
}

// maxScrapeHeadersBytes caps the total header size as the sum of
// len(key)+len(value) over canonicalized keys. The cap is exclusive.
const maxScrapeHeadersBytes = 8 * 1024

// maxScrapeListEntries caps IncludeTags, ExcludeTags, and
// ScrapeLocation.Languages at 100 entries each.
const maxScrapeListEntries = 100

// canonicalHeaders returns Headers with keys canonicalized via
// http.CanonicalHeaderKey (nil when empty), rejecting blank names,
// deny-listed names, canonicalization collisions, values containing '\r' or
// '\n', and totals >= 8KB.
func (o *ScrapeOptions) canonicalHeaders() (map[string]string, error) {
	if len(o.Headers) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(o.Headers))
	total := 0
	for k, v := range o.Headers {
		ck := http.CanonicalHeaderKey(k)
		if strings.TrimSpace(ck) == "" {
			return nil, fmt.Errorf("firecrawl: invalid header name %q: %w", k, search.ErrInvalidRequest)
		}
		if strings.ContainsAny(k, "\r\n") || strings.ContainsAny(ck, "\r\n") {
			return nil, fmt.Errorf("firecrawl: invalid header name %q: %w", k, search.ErrInvalidRequest)
		}
		if _, denied := deniedScrapeHeaders[ck]; denied {
			return nil, fmt.Errorf("firecrawl: forbidden header %q: %w", k, search.ErrInvalidRequest)
		}
		if _, dup := out[ck]; dup {
			return nil, fmt.Errorf("firecrawl: duplicate header %q after canonicalization: %w", ck, search.ErrInvalidRequest)
		}
		if strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("firecrawl: invalid header value for %q: %w", k, search.ErrInvalidRequest)
		}
		out[ck] = v
		total += len(ck) + len(v)
	}
	if total >= maxScrapeHeadersBytes {
		return nil, fmt.Errorf("firecrawl: headers exceed 8KB: %w", search.ErrInvalidRequest)
	}
	return out, nil
}

// applyTo merges the set options into payload and returns the canonicalized
// headers for the HTTP request (nil when none). Callers must validate
// first; it performs no validation itself.
func (o *ScrapeOptions) applyTo(payload map[string]any) map[string]string {
	if o == nil {
		return nil
	}
	if o.OnlyMainContent != nil {
		payload["onlyMainContent"] = *o.OnlyMainContent
	}
	if o.Timeout != nil {
		payload["timeout"] = *o.Timeout
	}
	if o.WaitFor != nil {
		payload["waitFor"] = *o.WaitFor
	}
	if o.Mobile != nil {
		payload["mobile"] = *o.Mobile
	}
	if o.SkipTLSVerification != nil {
		payload["skipTlsVerification"] = *o.SkipTLSVerification
	}
	if o.RemoveBase64Images != nil {
		payload["removeBase64Images"] = *o.RemoveBase64Images
	}
	if o.BlockAds != nil {
		payload["blockAds"] = *o.BlockAds
	}
	if o.Proxy != nil {
		payload["proxy"] = *o.Proxy
	}
	if o.StoreInCache != nil {
		payload["storeInCache"] = *o.StoreInCache
	}
	if o.MaxAge != nil {
		payload["maxAge"] = *o.MaxAge
	}
	if len(o.IncludeTags) > 0 {
		trimmed := make([]string, len(o.IncludeTags))
		for i, t := range o.IncludeTags {
			trimmed[i] = strings.TrimSpace(t)
		}
		payload["includeTags"] = trimmed
	}
	if len(o.ExcludeTags) > 0 {
		trimmed := make([]string, len(o.ExcludeTags))
		for i, t := range o.ExcludeTags {
			trimmed[i] = strings.TrimSpace(t)
		}
		payload["excludeTags"] = trimmed
	}
	if o.Location != nil {
		if loc := o.Location.payload(); loc != nil {
			payload["location"] = loc
		}
	}
	if o.ZeroDataRetention != nil {
		payload["zeroDataRetention"] = *o.ZeroDataRetention
	}
	if o.RedactPII != nil {
		payload["redactPII"] = *o.RedactPII
	}
	headers, _ := o.canonicalHeaders()
	if len(headers) > 0 {
		payload["headers"] = headers
	}
	return headers
}

// payload renders the location wire object ({"country", "languages"}), or
// nil when all-empty (Country blank and no Languages), in which case the
// caller omits the key. Country is normalized with Upper(TrimSpace).
func (l *ScrapeLocation) payload() map[string]any {
	if l == nil {
		return nil
	}
	country := strings.ToUpper(strings.TrimSpace(l.Country))
	if country == "" && len(l.Languages) == 0 {
		return nil
	}
	m := make(map[string]any, 2)
	if country != "" {
		m["country"] = country
	}
	if len(l.Languages) > 0 {
		trimmed := make([]string, len(l.Languages))
		for i, lang := range l.Languages {
			trimmed[i] = strings.TrimSpace(lang)
		}
		m["languages"] = trimmed
	}
	return m
}
