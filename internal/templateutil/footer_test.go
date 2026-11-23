package templateutil

import (
	"strings"
	"testing"
)

func TestValidateFooter(t *testing.T) {
	for _, tt := range []struct {
		text  string
		valid bool
	}{
		{"", true}, {"  ", true}, {"Terima kasih!", true}, {strings.Repeat("界", 60), true},
		{strings.Repeat("界", 61), false}, {"Thank you {{1}}", false}, {"Thank you {{name}}", false},
		{"Contact: https://example.test/a_b", true},
	} {
		if err := ValidateFooter(tt.text); (err == nil) != tt.valid {
			t.Errorf("ValidateFooter(%q)=%v, valid=%v", tt.text, err, tt.valid)
		}
	}
}
