package cluster

import (
	"strings"
	"testing"
)

func TestNodeDNSDefaultsToEmpty(t *testing.T) {
	dns := nodeDNS(Config{})
	if len(dns) != 0 {
		t.Errorf("dns = %q, want empty (stock resolv.conf) when --dns is unset", dns)
	}
}

func TestNodeDNSExplicitConfigWins(t *testing.T) {
	dns := nodeDNS(Config{DNS: []string{"10.0.0.53", "1.1.1.1"}})
	if got := strings.Join(dns, ","); got != "10.0.0.53,1.1.1.1" {
		t.Errorf("servers = %q, want the explicit list untouched", got)
	}
}

func TestValidateDNS(t *testing.T) {
	if err := validateDNS(Config{DNS: []string{"1.1.1.1", "fd00::53"}}); err != nil {
		t.Errorf("valid IPs rejected: %v", err)
	}
	if err := validateDNS(Config{}); err != nil {
		t.Errorf("empty DNS rejected: %v", err)
	}
	err := validateDNS(Config{DNS: []string{"dns.example.com"}})
	if err == nil || !strings.Contains(err.Error(), "dns.example.com") {
		t.Errorf("hostname accepted or unclear error: %v", err)
	}
	err = validateDNS(Config{DNS: []string{"1.1.1.1", "8.8.8.8", "9.9.9.9", "1.0.0.1"}})
	if err == nil || !strings.Contains(err.Error(), "at most 3") {
		t.Errorf("4 nameservers accepted or unclear error: %v", err)
	}
}
