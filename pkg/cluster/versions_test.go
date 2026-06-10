package cluster

import (
	"strings"
	"testing"
)

func TestResolveImage(t *testing.T) {
	cases := []struct {
		in      string
		want    string // substring of resolved image
		wantErr bool
	}{
		{in: "1.36", want: "kindest/node:v1.36.1@sha256:"},
		{in: "v1.36", want: "kindest/node:v1.36.1@sha256:"},
		{in: "1.32", want: "kindest/node:v1.32.11@sha256:"},
		{in: "1.34.8", want: "kindest/node:v1.34.8@sha256:"},
		{in: "1.34.2", want: "kindest/node:v1.34.2"}, // unpinned patch fallback
		{in: "1.19", wantErr: true},
		{in: "latest", wantErr: true},
	}
	for _, c := range cases {
		got, err := ResolveImage(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ResolveImage(%q): expected error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ResolveImage(%q): %v", c.in, err)
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("ResolveImage(%q) = %q, want substring %q", c.in, got, c.want)
		}
	}
	if def := SupportedVersions()[0]; def != DefaultK8sVersion {
		t.Errorf("newest supported version %q should equal default %q", def, DefaultK8sVersion)
	}
}
