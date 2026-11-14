package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseDNSDefaultsFieldPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, input, domain, source  string
		nameservers, search, options []string
		unset                        bool
	}{
		{
			name: "independent container defaults override host domain", input: `{"container":{"dns":{"nameservers":["10.0.0.53","fd00::53"],"domain":"nodes.test","searchDomains":["corp.test"],"options":["ndots:2"]}},"dns":{"domain":"host.test"},"registry":{"password":"DO-NOT-DISCLOSE"}}`,
			domain: "nodes.test", source: "container.dns.domain", nameservers: []string{"10.0.0.53", "fd00::53"}, search: []string{"corp.test"}, options: []string{"ndots:2"},
		},
		{
			name: "nameservers do not replace other defaults", input: `{"container":{"dns":{"nameservers":["1.1.1.1"],"searchDomains":["corp.test"],"options":["timeout:1"]}},"dns":{"domain":"host.test"}}`,
			domain: "host.test", source: "dns.domain fallback", nameservers: []string{"1.1.1.1"}, search: []string{"corp.test"}, options: []string{"timeout:1"},
		},
		{name: "1.6 fallback", input: `{"container":{"cpus":4,"memory":"1gb"},"dns":{"domain":"host.test"}}`, domain: "host.test", source: "dns.domain fallback"},
		{name: "no configuration", input: `{"container":{},"dns":{}}`, unset: true},
		{name: "null optional defaults", input: `{"container":{"dns":null},"dns":{"domain":null}}`, unset: true},
		{name: "explicit empty domain blocks fallback", input: `{"container":{"dns":{"domain":""}},"dns":{"domain":"host.test"}}`, source: "container.dns.domain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDNSDefaults([]byte(tc.input))
			if err != nil {
				t.Fatal(err)
			}
			if (got.Domain == nil) != tc.unset || (!tc.unset && *got.Domain != tc.domain) || got.DomainSource != tc.source {
				t.Fatalf("domain = %v (%s), want %q (%s), unset=%v", got.Domain, got.DomainSource, tc.domain, tc.source, tc.unset)
			}
			if !reflect.DeepEqual(got.Nameservers, tc.nameservers) || !reflect.DeepEqual(got.SearchDomains, tc.search) || !reflect.DeepEqual(got.Options, tc.options) {
				t.Fatalf("field defaults = %+v", got)
			}
		})
	}
}

func TestParseDNSDefaultsRejectsInvalidWithoutLeaking(t *testing.T) {
	for _, input := range []string{
		`{"password":"DO-NOT-DISCLOSE"`, `{}`, `null`, `[]`, `{"container":null}`,
		`{"container":{"dns":{"nameservers":["DO-NOT-DISCLOSE"]}}}`,
		`{"container":{"dns":{"options":"DO-NOT-DISCLOSE"}}}`,
		`{"container":{"dns":{"domain":42}}}`,
		`{"container":{"dns":{"nameservers":["1.1.1.1"]}}} {"password":"DO-NOT-DISCLOSE"}`,
		`{"container":{"dns":{"domain":"` + strings.Repeat("x", 1025) + `"}}}`,
		`{"container":{"dns":{"options":["` + strings.Repeat("x", 1025) + `"]}}}`,
		`{"container":{},"other":"` + strings.Repeat("x", dnsDefaultsOutputLimit) + `"}`,
		`{"container":{"dns":{"options":[` + strings.Repeat(`"ndots:2",`, 32) + `"ndots:2"]}}}`,
	} {
		_, err := parseDNSDefaults([]byte(input))
		if err == nil || strings.Contains(err.Error(), "DO-NOT-DISCLOSE") {
			t.Errorf("invalid response accepted or leaked: %v", err)
		}
	}
}

func dnsTestClient(t *testing.T, script string) *Client {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "container")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Client{Bin: bin}
}

func TestDNSDefaultsReadOnlyCommand(t *testing.T) {
	c := dnsTestClient(t, `test "$*" = 'system property list --format json' || exit 42
printf '%s' '{"container":{"dns":{"nameservers":["1.1.1.1"]}},"registry":{"auth":"DO-NOT-DISCLOSE"}}'
printf '%s' 'unrelated diagnostic DO-NOT-DISCLOSE' >&2`)
	got, err := c.DNSDefaults(time.Second)
	if err != nil || !reflect.DeepEqual(got.Nameservers, []string{"1.1.1.1"}) {
		t.Fatalf("defaults = %+v, error=%v", got, err)
	}
}

func TestDNSDefaultsErrorPrivacy(t *testing.T) {
	for _, script := range []string{
		`printf 'DO-NOT-DISCLOSE'; printf 'DO-NOT-DISCLOSE' >&2; exit 1`,
		`printf 'DO-NOT-DISCLOSE'`,
	} {
		_, err := dnsTestClient(t, script).DNSDefaults(time.Second)
		if err == nil || strings.Contains(err.Error(), "DO-NOT-DISCLOSE") {
			t.Fatalf("command error = %v", err)
		}
	}
}

func TestDNSDefaultsTimeoutAndOutputLimit(t *testing.T) {
	t.Run("timeout with inherited pipes", func(t *testing.T) {
		c := dnsTestClient(t, "sleep 10 &\nwait")
		start := time.Now()
		_, err := c.DNSDefaults(100 * time.Millisecond)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 4*time.Second {
			t.Fatalf("timeout: %v after %s", err, time.Since(start))
		}
	})
	t.Run("unbounded output canceled", func(t *testing.T) {
		c := dnsTestClient(t, `while :; do printf 'DO-NOT-DISCLOSE%.0s' 1 2 3 4 5 6 7 8 9 10; done`)
		start := time.Now()
		_, err := c.DNSDefaults(10 * time.Second)
		if err == nil || !strings.Contains(err.Error(), "64 KiB") || strings.Contains(err.Error(), "DO-NOT-DISCLOSE") || time.Since(start) > 4*time.Second {
			t.Fatalf("output limit: %v after %s", err, time.Since(start))
		}
	})
}
