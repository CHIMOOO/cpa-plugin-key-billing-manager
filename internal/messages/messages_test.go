package messages

import (
	"errors"
	"fmt"
	"testing"
)

func TestDescriptorsPreserveArgumentsAndWrappedErrors(t *testing.T) {
	message := New("Window name is required and must not exceed %d bytes", 128)
	if message.Key == "" || message.Params["v0"] != "128" || message.Text != "Window name is required and must not exceed 128 bytes" {
		t.Fatalf("message = %+v", message)
	}
	cause := errors.New("upstream diagnostic 50% 中文")
	err := Errorf("Read auth file: %w", cause)
	if !errors.Is(err, cause) || FromError(err).Params["v0"] != cause.Error() {
		t.Fatalf("wrapped error = %v, descriptor = %+v", err, FromError(err))
	}
	if raw := Literal(cause.Error()); raw.Key != "" || raw.Text != cause.Error() {
		t.Fatalf("raw message was interpreted: %+v", raw)
	}
}

func TestRawErrorsAreNotTranslatedByTextOrUnwrappedPastContext(t *testing.T) {
	raw := errors.New("No email provided") // Coincides with a built-in label.
	if detail := FromError(raw); detail.Key != "" || detail.Text != raw.Error() {
		t.Fatalf("raw upstream error was treated as plugin copy: %+v", detail)
	}
	typed := Errorf("Read auth file: %w", errors.New("test cause"))
	wrapped := fmt.Errorf("additional context: %w", typed)
	if detail := FromError(wrapped); detail.Key != "" || detail.Text != wrapped.Error() {
		t.Fatalf("outer error context was lost: %+v", detail)
	}
}
