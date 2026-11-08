package iox_test

import (
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/iox"
)

func TestTailBufferKeepsLastBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		limit  int
		writes []string
		want   string
	}{
		{name: "under limit", limit: 10, writes: []string{"abc", "def"}, want: "abcdef"},
		{name: "overflow across writes", limit: 5, writes: []string{"abcd", "efgh"}, want: "defgh"},
		{name: "single write larger than limit", limit: 3, writes: []string{"abcdef"}, want: "def"},
		{name: "disabled", limit: 0, writes: []string{"abc"}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			buffer := iox.NewTailBuffer(tc.limit)
			for _, chunk := range tc.writes {
				n, err := buffer.Write([]byte(chunk))
				if err != nil || n != len(chunk) {
					t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
				}
			}
			if got := buffer.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}
