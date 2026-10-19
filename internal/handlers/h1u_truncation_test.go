package handlers

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// An emoji straddling byte 97 used to be cut in half by a byte slice, producing
// invalid UTF-8 that PostgreSQL rejects ("invalid byte sequence for encoding
// UTF8: 0xf0 0x2e 0x2e 0x2e"), which failed the whole contact-summary write.
func TestCoexistenceMessagePreviewIsRuneSafe(t *testing.T) {
	content := strings.Repeat("a", 96) + "😀😀😀 and more text after the emoji"
	preview := coexistenceMessagePreview("text", content)
	if !utf8.ValidString(preview) {
		t.Fatalf("preview is not valid UTF-8: %q", preview)
	}
	if got := utf8.RuneCountInString(preview); got != 100 {
		t.Fatalf("preview has %d runes, want 100", got)
	}
	if !strings.HasSuffix(preview, "...") {
		t.Fatalf("preview %q lacks the ellipsis", preview)
	}

	short := "short 😀 text"
	if got := coexistenceMessagePreview("text", short); got != short {
		t.Fatalf("short preview changed: %q", got)
	}
	if got := coexistenceMessagePreview("image", content); got != "[image]" {
		t.Fatalf("media preview = %q, want [image]", got)
	}
}

func TestStoredErrorTextsAreRuneSafe(t *testing.T) {
	long := errors.New(strings.Repeat("x", 1999) + "😀 trailing detail")

	stored := inboundContinuationError(long)
	if !utf8.ValidString(stored) {
		t.Fatalf("inbound continuation error is not valid UTF-8")
	}
	if got := utf8.RuneCountInString(stored); got > 2000 {
		t.Fatalf("inbound continuation error has %d runes, want <= 2000", got)
	}
	if inboundContinuationError(nil) != "" {
		t.Fatalf("nil error must store as empty")
	}

	media := coexistenceMediaErrorText(long)
	if !utf8.ValidString(media) {
		t.Fatalf("coexistence media error is not valid UTF-8")
	}
	if got := utf8.RuneCountInString(media); got > 2000 {
		t.Fatalf("coexistence media error has %d runes, want <= 2000", got)
	}
}
