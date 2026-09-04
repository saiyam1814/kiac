package cluster

import (
	"slices"
	"testing"
	"time"
)

func TestCiliumInstallArgs(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    []string
	}{
		{
			name:    "uses cluster wait budget",
			timeout: 15 * time.Minute,
			want:    []string{"install", "--wait", "--wait-duration", "15m0s"},
		},
		{
			name: "keeps cilium default without a budget",
			want: []string{"install", "--wait"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ciliumInstallArgs(tt.timeout); !slices.Equal(got, tt.want) {
				t.Fatalf("ciliumInstallArgs(%s) = %q, want %q", tt.timeout, got, tt.want)
			}
		})
	}
}
