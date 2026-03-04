package modelid

import "testing"

func TestStripDisplayContextSuffixes(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"claude-sonnet-4-6[1m]", "claude-sonnet-4-6"},
		{"deepseek-v4-pro[1000k]", "deepseek-v4-pro"},
		{"deepseek-v4-pro[1000k][1m]", "deepseek-v4-pro"},
		{"gpt-5.4", "gpt-5.4"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := StripDisplayContextSuffixes(tt.input); got != tt.want {
				t.Fatalf("StripDisplayContextSuffixes(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
