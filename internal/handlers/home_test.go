package handlers

import (
	"testing"

	"logo23007go/internal/logo"
)

func TestNegotiateImageFormat(t *testing.T) {
	tests := []struct {
		accept string
		want   logo.OutputFormat
	}{
		{"", logo.FormatJPEG},
		{"*/*", logo.FormatJPEG},
		{"image/jpeg", logo.FormatJPEG},
		{"image/webp", logo.FormatWebP},
		{"image/avif,image/webp,image/apng,image/*,*/*;q=0.8", logo.FormatWebP},
		{"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8", logo.FormatWebP},
		{"IMAGE/WEBP;q=0.9,image/jpeg;q=0.8", logo.FormatWebP},
	}
	for _, tt := range tests {
		if got := negotiateImageFormat(tt.accept); got != tt.want {
			t.Fatalf("negotiateImageFormat(%q)=%s, want %s", tt.accept, got, tt.want)
		}
	}
}
