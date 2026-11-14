package cluster

import (
	"fmt"
	"net"
)

// maxNodeDNSServers is resolv.conf's own nameserver cap: glibc (and
// most other resolvers) read at most 3 "nameserver" lines and ignore
// the rest.
const maxNodeDNSServers = 3

// nodeDNS returns the nameserver list for a cluster's node VMs. An
// explicit --dns/dns: list replaces the runtime's nameservers. On
// apple/container, domain, search domains and resolver options retain
// their independently configured defaults. Without it, nodeDNS returns
// empty and the runtime chooses the resolver configuration.
func nodeDNS(cfg Config) []string {
	return cfg.DNS
}

// validateDNS rejects a non-IP --dns value, or more entries than
// resolv.conf honors, before any VM boots.
func validateDNS(cfg Config) error {
	if len(cfg.DNS) > maxNodeDNSServers {
		return fmt.Errorf("--dns accepts at most %d nameservers (resolv.conf ignores the rest); got %d", maxNodeDNSServers, len(cfg.DNS))
	}
	for _, s := range cfg.DNS {
		if net.ParseIP(s) == nil {
			return fmt.Errorf("invalid --dns %q: use nameserver IP addresses (e.g. --dns 1.1.1.1)", s)
		}
	}
	return nil
}
