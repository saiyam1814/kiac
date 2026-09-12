package runtime

import "testing"

func TestParsePublish(t *testing.T) {
	cases := []struct {
		name  string
		spec  string
		want  string
		isErr bool
	}{
		{name: "host and container ports", spec: "8080:80", want: "8080:80"},
		{name: "ipv4 host ip", spec: "127.0.0.1:8080:80", want: "127.0.0.1:8080:80"},
		{name: "ipv6 host ip", spec: "[::1]:8080:80", want: "[::1]:8080:80"},
		{name: "udp protocol lower-cased", spec: "127.0.0.1:5353:53/UDP", want: "127.0.0.1:5353:53/udp"},
		{name: "missing container port", spec: "127.0.0.1:8080", isErr: true},
		{name: "invalid port", spec: "abc:80", isErr: true},
		{name: "unbracketed ipv6", spec: "::1:8080:80", isErr: true},
		{name: "invalid protocol", spec: "8080:80/sctp", isErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePublish(tc.spec)
			if tc.isErr {
				if err == nil {
					t.Fatalf("ParsePublish(%q) unexpectedly succeeded", tc.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePublish(%q): %v", tc.spec, err)
			}
			if got.String() != tc.want {
				t.Fatalf("ParsePublish(%q) = %q, want %q", tc.spec, got.String(), tc.want)
			}
		})
	}
}

func TestValidatePublishes(t *testing.T) {
	if err := ValidatePublishes([]string{"127.0.0.1:8080:80", "[::1]:8443:443/tcp"}); err != nil {
		t.Fatalf("valid publishes rejected: %v", err)
	}
	if err := ValidatePublishes([]string{"127.0.0.1:abc:80"}); err == nil {
		t.Fatal("invalid publish accepted")
	}
}
