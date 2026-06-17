package storage

import (
	"errors"
	"testing"
)

func TestValidateObjectName(t *testing.T) {
	tests := []struct {
		name    string
		object  string
		wantErr bool
	}{
		{"simple", "file.txt", false},
		{"nested", "a/b/c.html", false},
		{"dotfile", ".env", false},
		{"dots in name not segment", "a..b/c", false},
		{"empty", "", true},
		{"absolute", "/etc/passwd", true},
		{"traversal leading", "../secret", true},
		{"traversal middle", "a/../../etc/passwd", true},
		{"traversal trailing", "a/b/..", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateObjectName(tt.object)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tt.object)
				}
				if !errors.Is(err, ErrInvalidObjectName) {
					t.Fatalf("expected ErrInvalidObjectName for %q, got %v", tt.object, err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error for %q: %v", tt.object, err)
			}
		})
	}
}
