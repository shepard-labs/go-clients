package email

import (
	"errors"
	"testing"
)

func TestMessageValidate(t *testing.T) {
	valid := Message{
		From:     "from@example.com",
		To:       []string{"to@example.com"},
		Subject:  "hi",
		TextBody: "body",
	}

	tests := []struct {
		name    string
		mutate  func(m *Message)
		wantErr bool
	}{
		{"valid", func(*Message) {}, false},
		{"valid html only", func(m *Message) { m.TextBody = ""; m.HTMLBody = "<p>x</p>" }, false},
		{"missing from", func(m *Message) { m.From = "" }, true},
		{"no recipients", func(m *Message) { m.To = nil }, true},
		{"missing subject", func(m *Message) { m.Subject = "" }, true},
		{"no body", func(m *Message) { m.TextBody = ""; m.HTMLBody = "" }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := valid
			tt.mutate(&m)
			err := m.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !errors.Is(err, ErrInvalidMessage) {
					t.Fatalf("expected ErrInvalidMessage, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestMessageValidateNil(t *testing.T) {
	var m *Message
	if err := m.Validate(); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("expected ErrInvalidMessage for nil message, got %v", err)
	}
}
