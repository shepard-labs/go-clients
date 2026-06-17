// Package postmark implements the email.Sender interface backed by the
// Postmark API.
//
// The server token is bound at construction. Provider-specific operations
// beyond the shared Sender interface (server info, domain verification) are
// available as methods on *Client and can be reached via a type assertion on
// the returned email.Sender.
package postmark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/shepard-labs/go-clients/email"
)

const baseURL = "https://api.postmarkapp.com"

// Ensure *Client satisfies the shared interface.
var _ email.Sender = (*Client)(nil)

// Client implements email.Sender backed by the Postmark API. The server token
// is bound at construction.
type Client struct {
	httpClient  *http.Client
	logger      *zap.Logger
	serverToken string
	baseURL     string
}

// New constructs a Postmark-backed email.Sender bound to serverToken.
func New(serverToken string, logger *zap.Logger) email.Sender {
	return &Client{
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		logger:      logger,
		serverToken: serverToken,
		baseURL:     baseURL,
	}
}

// postmarkEmail is the wire representation of an outbound message. Postmark
// uses PascalCase JSON field names and a single comma-separated recipient list.
type postmarkEmail struct {
	From          string            `json:"From"`
	To            string            `json:"To"`
	Cc            string            `json:"Cc,omitempty"`
	Bcc           string            `json:"Bcc,omitempty"`
	ReplyTo       string            `json:"ReplyTo,omitempty"`
	Subject       string            `json:"Subject"`
	HtmlBody      string            `json:"HtmlBody,omitempty"`
	TextBody      string            `json:"TextBody,omitempty"`
	Tag           string            `json:"Tag,omitempty"`
	TrackOpens    bool              `json:"TrackOpens"`
	Headers       []wireHeader      `json:"Headers,omitempty"`
	Metadata      map[string]string `json:"Metadata,omitempty"`
	MessageStream string            `json:"MessageStream,omitempty"`
}

type wireHeader struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

// SendEmailResponse represents Postmark's send email response.
type SendEmailResponse struct {
	To          string `json:"To"`
	SubmittedAt string `json:"SubmittedAt"`
	MessageID   string `json:"MessageID"`
	ErrorCode   int    `json:"ErrorCode"`
	Message     string `json:"Message"`
}

// ServerInfo represents Postmark server information.
type ServerInfo struct {
	ID                         int    `json:"ID"`
	Name                       string `json:"Name"`
	Color                      string `json:"Color"`
	SmtpApiActivated           bool   `json:"SmtpApiActivated"`
	RawEmailEnabled            bool   `json:"RawEmailEnabled"`
	DeliveryType               string `json:"DeliveryType"`
	ServerLink                 string `json:"ServerLink"`
	InboundAddress             string `json:"InboundAddress"`
	InboundHookUrl             string `json:"InboundHookUrl"`
	BounceHookUrl              string `json:"BounceHookUrl"`
	OpenHookUrl                string `json:"OpenHookUrl"`
	DeliveryHookUrl            string `json:"DeliveryHookUrl"`
	PostFirstOpenOnly          bool   `json:"PostFirstOpenOnly"`
	InboundDomain              string `json:"InboundDomain"`
	InboundHash                string `json:"InboundHash"`
	InboundSpamThreshold       int    `json:"InboundSpamThreshold"`
	TrackOpens                 bool   `json:"TrackOpens"`
	TrackLinks                 string `json:"TrackLinks"`
	IncludeBounceContentInHook bool   `json:"IncludeBounceContentInHook"`
	ClickHookUrl               string `json:"ClickHookUrl"`
	EnableSmtpApiErrorHooks    bool   `json:"EnableSmtpApiErrorHooks"`
}

// Domain represents a Postmark domain.
type Domain struct {
	ID                         int    `json:"ID"`
	Name                       string `json:"Name"`
	SPFVerified                bool   `json:"SPFVerified"`
	DKIMVerified               bool   `json:"DKIMVerified"`
	WeakDKIM                   bool   `json:"WeakDKIM"`
	ReturnPathVerified         bool   `json:"ReturnPathDomainVerified"`
	DKIMHost                   string `json:"DKIMHost"`
	DKIMTextValue              string `json:"DKIMTextValue"`
	DKIMPendingHost            string `json:"DKIMPendingHost"`
	DKIMPendingTextValue       string `json:"DKIMPendingTextValue"`
	DKIMRevokedHost            string `json:"DKIMRevokedHost"`
	DKIMRevokedTextValue       string `json:"DKIMRevokedTextValue"`
	ReturnPathDomain           string `json:"ReturnPathDomain"`
	ReturnPathDomainCNAMEValue string `json:"ReturnPathDomainCNAMEValue"`
}

// PostmarkError represents an API error.
type PostmarkError struct {
	ErrorCode int    `json:"ErrorCode"`
	Message   string `json:"Message"`
}

func (e *PostmarkError) Error() string {
	return fmt.Sprintf("postmark error %d: %s", e.ErrorCode, e.Message)
}

// IsRateLimitError reports whether the error is a rate-limit error.
func (e *PostmarkError) IsRateLimitError() bool {
	return e.ErrorCode == 429
}

// IsInvalidEmailError reports whether the error indicates an invalid email.
func (e *PostmarkError) IsInvalidEmailError() bool {
	return e.ErrorCode == 300 || e.ErrorCode == 406
}

// doRequest performs an HTTP request with retry logic against the Postmark API.
func (c *Client) doRequest(ctx context.Context, method, endpoint string, body interface{}) ([]byte, error) {
	url := c.baseURL + endpoint

	// Marshal once; a fresh reader is created per attempt so retries resend the
	// full body rather than an already-consumed reader.
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		bodyBytes = b
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		var reqBody io.Reader
		if bodyBytes != nil {
			reqBody = bytes.NewReader(bodyBytes)
		}

		req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Postmark-Server-Token", c.serverToken)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
			continue
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			lastErr = fmt.Errorf("failed to read response: %w", err)
			continue
		}

		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("server error: %d", resp.StatusCode)
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}

		if resp.StatusCode >= 400 {
			var pmErr PostmarkError
			if err := json.Unmarshal(respBody, &pmErr); err != nil {
				return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
			}
			return nil, &pmErr
		}

		return respBody, nil
	}

	return nil, fmt.Errorf("request failed after retries: %w", lastErr)
}

// Send sends an email using the Postmark API. It implements email.Sender.
//
// Postmark accepts a single comma-separated recipient list per field, so the
// To/Cc/Bcc/ReplyTo slices are joined. Only the first Tag is forwarded, as
// Postmark supports a single tag per message.
func (c *Client) Send(ctx context.Context, msg *email.Message) (*email.SendResult, error) {
	if err := msg.Validate(); err != nil {
		return nil, err
	}

	pm := postmarkEmail{
		From:       msg.From,
		To:         strings.Join(msg.To, ","),
		Cc:         strings.Join(msg.Cc, ","),
		Bcc:        strings.Join(msg.Bcc, ","),
		ReplyTo:    strings.Join(msg.ReplyTo, ","),
		Subject:    msg.Subject,
		HtmlBody:   msg.HTMLBody,
		TextBody:   msg.TextBody,
		TrackOpens: true,
	}
	if len(msg.Tags) > 0 {
		pm.Tag = msg.Tags[0].Name
	}
	for _, h := range msg.Headers {
		pm.Headers = append(pm.Headers, wireHeader{Name: h.Name, Value: h.Value})
	}

	respBody, err := c.doRequest(ctx, "POST", "/email", &pm)
	if err != nil {
		return nil, err
	}

	var resp SendEmailResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &email.SendResult{MessageID: resp.MessageID}, nil
}

// VerifyAuth validates the bound server token. It implements email.Sender.
func (c *Client) VerifyAuth(ctx context.Context) error {
	_, err := c.GetServerInfo(ctx)
	return err
}

// GetServerInfo retrieves server information for the bound token.
func (c *Client) GetServerInfo(ctx context.Context) (*ServerInfo, error) {
	respBody, err := c.doRequest(ctx, "GET", "/server", nil)
	if err != nil {
		return nil, err
	}

	var resp ServerInfo
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &resp, nil
}

// GetDomain retrieves domain details.
func (c *Client) GetDomain(ctx context.Context, domainID int) (*Domain, error) {
	endpoint := fmt.Sprintf("/domains/%d", domainID)
	return c.domainRequest(ctx, "GET", endpoint)
}

// VerifyDKIM triggers DKIM verification for a domain.
func (c *Client) VerifyDKIM(ctx context.Context, domainID int) (*Domain, error) {
	endpoint := fmt.Sprintf("/domains/%d/verifyDkim", domainID)
	return c.domainRequest(ctx, "PUT", endpoint)
}

// VerifyReturnPath triggers return path verification for a domain.
func (c *Client) VerifyReturnPath(ctx context.Context, domainID int) (*Domain, error) {
	endpoint := fmt.Sprintf("/domains/%d/verifyReturnPath", domainID)
	return c.domainRequest(ctx, "PUT", endpoint)
}

func (c *Client) domainRequest(ctx context.Context, method, endpoint string) (*Domain, error) {
	respBody, err := c.doRequest(ctx, method, endpoint, nil)
	if err != nil {
		return nil, err
	}

	var resp Domain
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &resp, nil
}
