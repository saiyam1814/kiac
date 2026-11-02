package runtime

import (
	"errors"
	"strings"
	"testing"
)

func TestRequireNestedVirtualization(t *testing.T) {
	probeErr := errors.New("capability API unavailable")
	for _, tc := range []struct {
		name      string
		supported bool
		err       error
		want      string
	}{
		{name: "supported", supported: true},
		{name: "unsupported", want: "this host does not support --virtualization"},
		{name: "probe failed", err: probeErr, want: "cannot verify host support"},
		{name: "probe error overrides result", supported: true, err: probeErr, want: "cannot verify host support"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireNestedVirtualization(func() (bool, error) { return tc.supported, tc.err })
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "macOS 15+") {
				t.Fatalf("error = %v, want %q and platform requirements", err, tc.want)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("error %v does not wrap probe failure %v", err, tc.err)
			}
		})
	}
}
