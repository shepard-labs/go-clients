// Package ses implements the email.Sender interface backed by Amazon SES v2.
//
// Credentials are bound at construction. Provider-specific operations beyond
// the shared Sender interface (identity management, account introspection)
// are available as methods on *Client and can be reached via a type assertion
// on the returned email.Sender.
package ses

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awsses "github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/smithy-go"
	"go.uber.org/zap"

	"github.com/shepard-labs/go-clients/email"
)

// Ensure *Client satisfies the shared interface.
var _ email.Sender = (*Client)(nil)

// Credentials holds the AWS credentials required to authenticate SES API calls.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	Region          string
}

// Client implements email.Sender backed by Amazon SES v2. The bound
// credentials are used to construct an SES client per call, reusing a shared
// HTTP client for connection pooling.
type Client struct {
	httpClient  *http.Client
	logger      *zap.Logger
	creds       Credentials
	serviceTag  string
	buildClient func() sesClient
}

type sesClient interface {
	SendEmail(context.Context, *awsses.SendEmailInput, ...func(*awsses.Options)) (*awsses.SendEmailOutput, error)
	CreateEmailIdentity(context.Context, *awsses.CreateEmailIdentityInput, ...func(*awsses.Options)) (*awsses.CreateEmailIdentityOutput, error)
	GetEmailIdentity(context.Context, *awsses.GetEmailIdentityInput, ...func(*awsses.Options)) (*awsses.GetEmailIdentityOutput, error)
	DeleteEmailIdentity(context.Context, *awsses.DeleteEmailIdentityInput, ...func(*awsses.Options)) (*awsses.DeleteEmailIdentityOutput, error)
	PutEmailIdentityDkimAttributes(context.Context, *awsses.PutEmailIdentityDkimAttributesInput, ...func(*awsses.Options)) (*awsses.PutEmailIdentityDkimAttributesOutput, error)
	GetAccount(context.Context, *awsses.GetAccountInput, ...func(*awsses.Options)) (*awsses.GetAccountOutput, error)
}

// New constructs an SES-backed email.Sender bound to creds.
//
// serviceTag is an optional label applied to provider-side metadata where
// supported; pass "" to omit it. It currently has no effect on SES sends and
// is accepted for parity with other providers.
func New(creds Credentials, serviceTag string, logger *zap.Logger) email.Sender {
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		logger:     logger,
		creds:      creds,
		serviceTag: serviceTag,
	}
}

// IdentityInfo represents information about an SES email identity.
type IdentityInfo struct {
	IdentityName             string
	IdentityType             string
	VerifiedForSendingStatus bool
	DkimAttributes           DkimAttributes
}

// DkimAttributes represents DKIM signing attributes for an identity.
type DkimAttributes struct {
	SigningEnabled          bool
	Status                  string
	Tokens                  []string
	SigningAttributesOrigin string
}

// AccountInfo represents SES account sending details.
type AccountInfo struct {
	SendingEnabled          bool
	ProductionAccessEnabled bool
	DailyMax                float64
	MaxSendRate             float64
	SentLast24Hours         float64
}

// SESError represents an Amazon SES API error.
type SESError struct {
	Code    string
	Message string
}

func (e *SESError) Error() string {
	return fmt.Sprintf("ses error %s: %s", e.Code, e.Message)
}

// IsNotFoundException reports whether the error is a not-found error.
func (e *SESError) IsNotFoundException() bool {
	return e.Code == "NotFoundException"
}

// IsThrottlingError reports whether the error is a throttling or rate-limit error.
func (e *SESError) IsThrottlingError() bool {
	return e.Code == "TooManyRequestsException"
}

// IsInvalidEmailError reports whether the error indicates a permanent rejection
// of the recipient address. Real delivery failures are usually reported
// asynchronously via SNS bounce events.
func (e *SESError) IsInvalidEmailError() bool {
	return e.Code == "MessageRejected"
}

// IsConfigurationError reports whether the error is caused by a sender-side
// configuration problem.
func (e *SESError) IsConfigurationError() bool {
	return e.Code == "MailFromDomainNotVerifiedException" ||
		e.Code == "AccountSuspendedException" ||
		e.Code == "SendingPausedException" ||
		e.Code == "ConfigurationSetDoesNotExistException" ||
		e.Code == "ConfigurationSetSendingPausedException"
}

// buildAWSClient builds an *awsses.Client using the bound static credentials.
// The shared httpClient is reused across calls for connection pooling.
func (c *Client) buildAWSClient() *awsses.Client {
	cfg := aws.Config{
		Region: c.creds.Region,
		Credentials: credentials.NewStaticCredentialsProvider(
			c.creds.AccessKeyID,
			c.creds.SecretAccessKey,
			"",
		),
		HTTPClient: c.httpClient,
	}
	return awsses.NewFromConfig(cfg)
}

func (c *Client) sesClient() sesClient {
	if c.buildClient != nil {
		return c.buildClient()
	}
	return c.buildAWSClient()
}

// wrapSESError converts an AWS SDK error into a typed *SESError when possible.
func wrapSESError(err error) error {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return &SESError{
			Code:    apiErr.ErrorCode(),
			Message: apiErr.ErrorMessage(),
		}
	}
	return err
}

// identityInfoFromOutput maps common identity fields from SES v2 responses.
func identityInfoFromOutput(name string, identityType types.IdentityType, verified bool, dkim *types.DkimAttributes) *IdentityInfo {
	info := &IdentityInfo{
		IdentityName:             name,
		VerifiedForSendingStatus: verified,
	}

	switch identityType {
	case types.IdentityTypeEmailAddress:
		info.IdentityType = "EMAIL_ADDRESS"
	case types.IdentityTypeDomain:
		info.IdentityType = "DOMAIN"
	case types.IdentityTypeManagedDomain:
		info.IdentityType = "MANAGED_DOMAIN"
	}

	if dkim != nil {
		info.DkimAttributes = DkimAttributes{
			SigningEnabled:          dkim.SigningEnabled,
			Status:                  string(dkim.Status),
			Tokens:                  dkim.Tokens,
			SigningAttributesOrigin: string(dkim.SigningAttributesOrigin),
		}
	}

	return info
}

// Send sends an email through Amazon SES v2. It implements email.Sender.
func (c *Client) Send(ctx context.Context, msg *email.Message) (*email.SendResult, error) {
	if err := msg.Validate(); err != nil {
		return nil, err
	}

	client := c.sesClient()

	input := &awsses.SendEmailInput{
		FromEmailAddress: aws.String(msg.From),
		Destination: &types.Destination{
			ToAddresses:  msg.To,
			CcAddresses:  msg.Cc,
			BccAddresses: msg.Bcc,
		},
		Content: &types.EmailContent{
			Simple: &types.Message{
				Subject: &types.Content{
					Data:    aws.String(msg.Subject),
					Charset: aws.String("UTF-8"),
				},
				Body: &types.Body{},
			},
		},
		ReplyToAddresses: msg.ReplyTo,
	}

	if msg.HTMLBody != "" {
		input.Content.Simple.Body.Html = &types.Content{
			Data:    aws.String(msg.HTMLBody),
			Charset: aws.String("UTF-8"),
		}
	}
	if msg.TextBody != "" {
		input.Content.Simple.Body.Text = &types.Content{
			Data:    aws.String(msg.TextBody),
			Charset: aws.String("UTF-8"),
		}
	}

	for _, tag := range msg.Tags {
		input.EmailTags = append(input.EmailTags, types.MessageTag{
			Name:  aws.String(tag.Name),
			Value: aws.String(tag.Value),
		})
	}

	for _, h := range msg.Headers {
		input.Content.Simple.Headers = append(input.Content.Simple.Headers, types.MessageHeader{
			Name:  aws.String(h.Name),
			Value: aws.String(h.Value),
		})
	}

	output, err := client.SendEmail(ctx, input)
	if err != nil {
		return nil, wrapSESError(err)
	}

	return &email.SendResult{
		MessageID: aws.ToString(output.MessageId),
	}, nil
}

// VerifyAuth validates the bound AWS credentials by calling GetAccount.
// It implements email.Sender.
func (c *Client) VerifyAuth(ctx context.Context) error {
	_, err := c.GetAccount(ctx)
	return err
}

// CreateEmailIdentity creates and begins verification of an email identity
// (address or domain).
func (c *Client) CreateEmailIdentity(ctx context.Context, identity string) (*IdentityInfo, error) {
	client := c.sesClient()

	output, err := client.CreateEmailIdentity(ctx, &awsses.CreateEmailIdentityInput{
		EmailIdentity: aws.String(identity),
	})
	if err != nil {
		return nil, wrapSESError(err)
	}

	return identityInfoFromOutput(identity, output.IdentityType, output.VerifiedForSendingStatus, output.DkimAttributes), nil
}

// GetEmailIdentity retrieves verification and DKIM information for an identity.
func (c *Client) GetEmailIdentity(ctx context.Context, identity string) (*IdentityInfo, error) {
	client := c.sesClient()

	output, err := client.GetEmailIdentity(ctx, &awsses.GetEmailIdentityInput{
		EmailIdentity: aws.String(identity),
	})
	if err != nil {
		return nil, wrapSESError(err)
	}

	return identityInfoFromOutput(identity, output.IdentityType, output.VerifiedForSendingStatus, output.DkimAttributes), nil
}

// DeleteEmailIdentity removes an email identity from SES.
func (c *Client) DeleteEmailIdentity(ctx context.Context, identity string) error {
	client := c.sesClient()

	_, err := client.DeleteEmailIdentity(ctx, &awsses.DeleteEmailIdentityInput{
		EmailIdentity: aws.String(identity),
	})
	return wrapSESError(err)
}

// PutEmailIdentityDkimAttributes enables or disables DKIM signing for an identity.
func (c *Client) PutEmailIdentityDkimAttributes(ctx context.Context, identity string, signingEnabled bool) error {
	client := c.sesClient()

	_, err := client.PutEmailIdentityDkimAttributes(ctx, &awsses.PutEmailIdentityDkimAttributesInput{
		EmailIdentity:  aws.String(identity),
		SigningEnabled: signingEnabled,
	})
	return wrapSESError(err)
}

// GetAccount retrieves sending quota and status for the SES account.
func (c *Client) GetAccount(ctx context.Context) (*AccountInfo, error) {
	client := c.sesClient()

	output, err := client.GetAccount(ctx, &awsses.GetAccountInput{})
	if err != nil {
		return nil, wrapSESError(err)
	}

	info := &AccountInfo{
		SendingEnabled:          output.SendingEnabled,
		ProductionAccessEnabled: output.ProductionAccessEnabled,
	}

	if output.SendQuota != nil {
		info.DailyMax = output.SendQuota.Max24HourSend
		info.MaxSendRate = output.SendQuota.MaxSendRate
		info.SentLast24Hours = output.SendQuota.SentLast24Hours
	}

	return info, nil
}
