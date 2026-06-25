package twitter

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// chunkSize is the maximum bytes per media-append segment.
const chunkSize = 4 * 1024 * 1024

// UploadMedia uploads media via the chunked flow (initialize → append →
// finalize → poll STATUS) and returns the processed Media. It is the only
// exported media entry point; the per-step helpers are unexported so callers
// cannot misuse the sequence.
//
// mediaType must be a spec enum MIME value (note image/jpg is invalid; use
// image/jpeg). category is required and must be one of tweet_image,
// tweet_video, tweet_gif.
//
// On any failure after initialize, the returned error names the media id so a
// half-populated upload can be diagnosed.
func (c *Client) UploadMedia(ctx context.Context, data []byte, mediaType, category string) (*Media, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: media data required", ErrInvalidRequest)
	}
	if mediaType == "" {
		return nil, fmt.Errorf("%w: media_type required", ErrInvalidRequest)
	}
	switch category {
	case "tweet_image", "tweet_video", "tweet_gif":
	default:
		return nil, fmt.Errorf("%w: media_category %q must be tweet_image, tweet_video, or tweet_gif", ErrInvalidRequest, category)
	}

	m, err := c.initMediaUpload(ctx, data, mediaType, category)
	if err != nil {
		return nil, err
	}
	mediaID := m.ID

	var segment int
	for offset := 0; offset < len(data); offset += chunkSize {
		end := min(offset+chunkSize, len(data))
		if err := c.appendMediaUpload(ctx, mediaID, segment, data[offset:end]); err != nil {
			return nil, fmt.Errorf("media %s: append segment %d: %w", mediaID, segment, err)
		}
		segment++
	}

	final, err := c.finalizeMediaUpload(ctx, mediaID)
	if err != nil {
		return nil, fmt.Errorf("media %s: finalize: %w", mediaID, err)
	}

	return c.pollMediaUpload(ctx, final)
}

// pollMediaUpload drives the processing-status state machine until the media is
// ready, failed, or the context is cancelled.
func (c *Client) pollMediaUpload(ctx context.Context, m *Media) (*Media, error) {
	for {
		if m.ProcessingInfo == nil || m.ProcessingInfo.State == "succeeded" {
			return m, nil
		}
		if m.ProcessingInfo.State == "failed" {
			return nil, fmt.Errorf("media %s: processing failed (state=%s)", m.ID, m.ProcessingInfo.State)
		}

		wait := time.Duration(max(m.ProcessingInfo.CheckAfterSecs, 1)) * time.Second
		if !c.sleeper(ctx, wait) {
			return nil, ctx.Err()
		}

		next, err := c.mediaUploadStatus(ctx, m.ID)
		if err != nil {
			return nil, fmt.Errorf("media %s: status: %w", m.ID, err)
		}
		m = next
	}
}

// initMediaUpload starts a chunked upload session. POST /2/media/upload/initialize.
func (c *Client) initMediaUpload(ctx context.Context, data []byte, mediaType, category string) (*Media, error) {
	req := initMediaRequest{
		MediaType:     mediaType,
		TotalBytes:    len(data),
		MediaCategory: category,
	}
	var env dataEnvelope[Media]
	if err := c.doJSON(ctx, http.MethodPost, "/2/media/upload/initialize", req, &env); err != nil {
		return nil, err
	}
	return &env.Data, nil
}

// appendMediaUpload uploads one chunk. POST /2/media/upload/{id}/append as
// multipart/form-data. The body is built fully into a buffer so the retry loop
// can resend identical bytes; X overwrites by segment index, so a resend is
// safe.
func (c *Client) appendMediaUpload(ctx context.Context, mediaID string, segment int, chunk []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	part, err := w.CreateFormFile("media", "blob")
	if err != nil {
		return fmt.Errorf("failed to create multipart part: %w", err)
	}
	if _, err := part.Write(chunk); err != nil {
		return fmt.Errorf("failed to write media chunk: %w", err)
	}
	if err := w.WriteField("segment_index", strconv.Itoa(segment)); err != nil {
		return fmt.Errorf("failed to write segment_index: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("failed to close multipart writer: %w", err)
	}

	_, err = c.doRequest(ctx, http.MethodPost, "/2/media/upload/"+mediaID+"/append", buf.Bytes(), w.FormDataContentType())
	return err
}

// finalizeMediaUpload completes the session. POST /2/media/upload/{id}/finalize.
func (c *Client) finalizeMediaUpload(ctx context.Context, mediaID string) (*Media, error) {
	var env dataEnvelope[Media]
	if err := c.doJSON(ctx, http.MethodPost, "/2/media/upload/"+mediaID+"/finalize", nil, &env); err != nil {
		return nil, err
	}
	return &env.Data, nil
}

// mediaUploadStatus polls processing status. GET /2/media/upload?media_id=&command=STATUS.
func (c *Client) mediaUploadStatus(ctx context.Context, mediaID string) (*Media, error) {
	q := url.Values{}
	q.Set("media_id", mediaID)
	q.Set("command", "STATUS")

	var env dataEnvelope[Media]
	if err := c.doJSON(ctx, http.MethodGet, "/2/media/upload?"+q.Encode(), nil, &env); err != nil {
		return nil, err
	}
	return &env.Data, nil
}

// SetMediaMetadata attaches alt text to an uploaded media item for
// accessibility. POST /2/media/metadata.
func (c *Client) SetMediaMetadata(ctx context.Context, mediaID, altTextValue string) error {
	if mediaID == "" {
		return fmt.Errorf("%w: mediaID required", ErrInvalidRequest)
	}

	req := metadataCreateRequest{
		ID:       mediaID,
		Metadata: metadataBody{AltText: &altText{Text: altTextValue}},
	}
	return c.doJSON(ctx, http.MethodPost, "/2/media/metadata", req, nil)
}
