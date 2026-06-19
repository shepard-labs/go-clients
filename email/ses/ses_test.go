package ses

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsses "github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/smithy-go"
	"go.uber.org/zap"

	"github.com/shepard-labs/go-clients/email"
)

type fakeSESClient struct {
	sendInput   *awsses.SendEmailInput
	sendOutput  *awsses.SendEmailOutput
	sendErr     error
	accountOut  *awsses.GetAccountOutput
	identityOut *awsses.GetEmailIdentityOutput
}

func (f *fakeSESClient) SendEmail(_ context.Context, in *awsses.SendEmailInput, _ ...func(*awsses.Options)) (*awsses.SendEmailOutput, error) {
	f.sendInput = in
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	return f.sendOutput, nil
}

func (f *fakeSESClient) CreateEmailIdentity(_ context.Context, in *awsses.CreateEmailIdentityInput, _ ...func(*awsses.Options)) (*awsses.CreateEmailIdentityOutput, error) {
	return &awsses.CreateEmailIdentityOutput{
		IdentityType:             types.IdentityTypeEmailAddress,
		VerifiedForSendingStatus: true,
		DkimAttributes: &types.DkimAttributes{
			SigningEnabled:          true,
			Status:                  types.DkimStatusSuccess,
			Tokens:                  []string{"a", "b"},
			SigningAttributesOrigin: types.DkimSigningAttributesOriginAwsSes,
		},
	}, nil
}

func (f *fakeSESClient) GetEmailIdentity(_ context.Context, in *awsses.GetEmailIdentityInput, _ ...func(*awsses.Options)) (*awsses.GetEmailIdentityOutput, error) {
	if f.identityOut != nil {
		return f.identityOut, nil
	}
	return &awsses.GetEmailIdentityOutput{IdentityType: types.IdentityTypeDomain}, nil
}

func (f *fakeSESClient) DeleteEmailIdentity(context.Context, *awsses.DeleteEmailIdentityInput, ...func(*awsses.Options)) (*awsses.DeleteEmailIdentityOutput, error) {
	return &awsses.DeleteEmailIdentityOutput{}, nil
}

func (f *fakeSESClient) PutEmailIdentityDkimAttributes(context.Context, *awsses.PutEmailIdentityDkimAttributesInput, ...func(*awsses.Options)) (*awsses.PutEmailIdentityDkimAttributesOutput, error) {
	return &awsses.PutEmailIdentityDkimAttributesOutput{}, nil
}

func (f *fakeSESClient) GetAccount(context.Context, *awsses.GetAccountInput, ...func(*awsses.Options)) (*awsses.GetAccountOutput, error) {
	if f.accountOut != nil {
		return f.accountOut, nil
	}
	return &awsses.GetAccountOutput{SendingEnabled: true}, nil
}

func TestSendBuildsSESRequest(t *testing.T) {
	fake := &fakeSESClient{sendOutput: &awsses.SendEmailOutput{MessageId: aws.String("msg-1")}}
	c := &Client{logger: zap.NewNop(), buildClient: func() sesClient { return fake }}

	res, err := c.Send(context.Background(), &email.Message{
		From:     "from@example.com",
		To:       []string{"to@example.com"},
		Cc:       []string{"cc@example.com"},
		Bcc:      []string{"bcc@example.com"},
		ReplyTo:  []string{"reply@example.com"},
		Subject:  "subject",
		HTMLBody: "<p>html</p>",
		TextBody: "text",
		Tags:     []email.Tag{{Name: "kind", Value: "welcome"}},
		Headers:  []email.Header{{Name: "X-Test", Value: "yes"}},
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if res.MessageID != "msg-1" {
		t.Fatalf("unexpected message id %q", res.MessageID)
	}
	if got := aws.ToString(fake.sendInput.FromEmailAddress); got != "from@example.com" {
		t.Fatalf("unexpected FromEmailAddress %q", got)
	}
	if got := aws.ToString(fake.sendInput.Content.Simple.Body.Html.Data); got != "<p>html</p>" {
		t.Fatalf("unexpected HTML body %q", got)
	}
	if len(fake.sendInput.EmailTags) != 1 || aws.ToString(fake.sendInput.EmailTags[0].Name) != "kind" {
		t.Fatalf("tags were not mapped: %#v", fake.sendInput.EmailTags)
	}
	if len(fake.sendInput.Content.Simple.Headers) != 1 || aws.ToString(fake.sendInput.Content.Simple.Headers[0].Name) != "X-Test" {
		t.Fatalf("headers were not mapped: %#v", fake.sendInput.Content.Simple.Headers)
	}
}

func TestSendWrapsSESError(t *testing.T) {
	fake := &fakeSESClient{sendErr: &smithy.GenericAPIError{Code: "MessageRejected", Message: "bad recipient"}}
	c := &Client{logger: zap.NewNop(), buildClient: func() sesClient { return fake }}

	_, err := c.Send(context.Background(), &email.Message{From: "from@example.com", To: []string{"to@example.com"}, Subject: "s", TextBody: "b"})
	var sesErr *SESError
	if !errors.As(err, &sesErr) {
		t.Fatalf("expected SESError, got %T %v", err, err)
	}
	if !sesErr.IsInvalidEmailError() {
		t.Fatalf("expected invalid email error, got %#v", sesErr)
	}
}

func TestIdentityAndAccountMethods(t *testing.T) {
	fake := &fakeSESClient{accountOut: &awsses.GetAccountOutput{
		SendingEnabled:          true,
		ProductionAccessEnabled: true,
		SendQuota: &types.SendQuota{
			Max24HourSend:   100,
			MaxSendRate:     10,
			SentLast24Hours: 5,
		},
	}}
	c := &Client{logger: zap.NewNop(), buildClient: func() sesClient { return fake }}

	created, err := c.CreateEmailIdentity(context.Background(), "a@example.com")
	if err != nil {
		t.Fatalf("CreateEmailIdentity failed: %v", err)
	}
	if created.IdentityType != "EMAIL_ADDRESS" || !created.DkimAttributes.SigningEnabled {
		t.Fatalf("unexpected identity info: %#v", created)
	}
	got, err := c.GetEmailIdentity(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("GetEmailIdentity failed: %v", err)
	}
	if got.IdentityType != "DOMAIN" {
		t.Fatalf("unexpected identity type %q", got.IdentityType)
	}
	if err := c.DeleteEmailIdentity(context.Background(), "example.com"); err != nil {
		t.Fatalf("DeleteEmailIdentity failed: %v", err)
	}
	if err := c.PutEmailIdentityDkimAttributes(context.Background(), "example.com", true); err != nil {
		t.Fatalf("PutEmailIdentityDkimAttributes failed: %v", err)
	}
	acct, err := c.GetAccount(context.Background())
	if err != nil {
		t.Fatalf("GetAccount failed: %v", err)
	}
	if !acct.SendingEnabled || acct.DailyMax != 100 || acct.MaxSendRate != 10 || acct.SentLast24Hours != 5 {
		t.Fatalf("unexpected account info: %#v", acct)
	}
	if err := c.VerifyAuth(context.Background()); err != nil {
		t.Fatalf("VerifyAuth failed: %v", err)
	}
}

func TestSESErrorHelpers(t *testing.T) {
	if !(&SESError{Code: "NotFoundException"}).IsNotFoundException() {
		t.Fatal("expected not found")
	}
	if !(&SESError{Code: "TooManyRequestsException"}).IsThrottlingError() {
		t.Fatal("expected throttling")
	}
	if !(&SESError{Code: "MailFromDomainNotVerifiedException"}).IsConfigurationError() {
		t.Fatal("expected configuration error")
	}
	if got := (&SESError{Code: "X", Message: "Y"}).Error(); got != "ses error X: Y" {
		t.Fatalf("unexpected error string %q", got)
	}
}

func TestNewAndDefaultClientBuilder(t *testing.T) {
	sender := New(Credentials{AccessKeyID: "id", SecretAccessKey: "secret", Region: "us-east-1"}, "svc", zap.NewNop())
	c, ok := sender.(*Client)
	if !ok {
		t.Fatalf("expected *Client, got %T", sender)
	}
	if c.httpClient == nil || c.logger == nil || c.serviceTag != "svc" {
		t.Fatalf("unexpected client: %#v", c)
	}
	if c.sesClient() == nil {
		t.Fatal("expected default SES client")
	}
}
