// Package email defines a provider-agnostic interface for sending
// transactional email, plus the shared types its methods operate on.
//
// Concrete implementations live in subpackages (ses, postmark). Each binds
// its provider credentials at construction time, so the Sender interface
// carries no authentication arguments.
//
// The interface intentionally covers only the capabilities common to all
// providers: sending a message and verifying that the bound credentials are
// usable. Provider-specific operations (SES identity management, Postmark
// domain verification, account/server introspection) remain as methods on
// the concrete client types and are reachable via a type assertion.
package email

import (
	"context"
	"errors"
	"fmt"
)

// ErrInvalidMessage indicates a Message failed validation before send. The
// wrapped detail names the offending field without echoing message content.
var ErrInvalidMessage = errors.New("invalid email message")

// Header is a custom MIME header attached to an outbound message.
type Header struct {
	Name  string
	Value string
}

// Tag is a key/value label for categorizing or filtering a message.
// Providers that do not support tagging ignore these.
type Tag struct {
	Name  string
	Value string
}

// Message is a provider-agnostic outbound email.
//
// Fields map to whatever the underlying provider supports; a provider that
// lacks a concept (e.g. Cc/Bcc) simply ignores the corresponding field.
// At least one of HTMLBody or TextBody should be set.
type Message struct {
	From     string
	To       []string
	Cc       []string
	Bcc      []string
	ReplyTo  []string
	Subject  string
	HTMLBody string
	TextBody string
	Tags     []Tag
	Headers  []Header

	// MessageStream selects the Postmark message stream the email is sent on.
	// It is Postmark-specific; providers that lack the concept ignore it.
	MessageStream string
}

// SendResult is the provider-agnostic outcome of a successful send.
type SendResult struct {
	// MessageID is the provider-assigned identifier for the accepted message.
	MessageID string
}

// Validate checks that the message carries the minimum fields every provider
// requires: a sender, at least one recipient, a subject, and a body. It
// returns an error wrapping ErrInvalidMessage naming the missing field. The
// error never includes message content.
func (m *Message) Validate() error {
	switch {
	case m == nil:
		return fmt.Errorf("%w: message is nil", ErrInvalidMessage)
	case m.From == "":
		return fmt.Errorf("%w: From is required", ErrInvalidMessage)
	case len(m.To) == 0:
		return fmt.Errorf("%w: at least one To recipient is required", ErrInvalidMessage)
	case m.Subject == "":
		return fmt.Errorf("%w: Subject is required", ErrInvalidMessage)
	case m.HTMLBody == "" && m.TextBody == "":
		return fmt.Errorf("%w: HTMLBody or TextBody is required", ErrInvalidMessage)
	}
	return nil
}

// Sender sends transactional email through a single, pre-configured provider.
//
// Implementations are safe for concurrent use. Credentials are bound when the
// implementation is constructed, so a Sender represents one authenticated
// provider account.
type Sender interface {
	// Send delivers msg. The returned SendResult carries the provider message
	// ID on success.
	Send(ctx context.Context, msg *Message) (*SendResult, error)

	// VerifyAuth confirms the bound credentials are valid and usable for
	// sending. It returns nil when the credentials are accepted.
	VerifyAuth(ctx context.Context) error
}
