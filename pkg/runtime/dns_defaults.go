package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"time"
)

const dnsDefaultsOutputLimit = 64 << 10

// DNSDefaults contains only the resolver defaults relevant to new ordinary
// apple/container nodes. It is not a guest resolver or a reachability check.
// Empty nameservers mean that the runtime chooses its network resolver.
type DNSDefaults struct {
	Nameservers   []string
	Domain        *string
	DomainSource  string
	SearchDomains []string
	Options       []string
}

// DNSDefaults reads the merged configuration through the same CLI command in
// apple/container 1.6 and newer. It never reads source config files, starts the
// service, or returns unrelated settings or captured CLI output in errors.
func (c *Client) DNSDefaults(timeout time.Duration) (DNSDefaults, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Bin, "system", "property", "list", "--format", "json")
	cmd.WaitDelay = pipeWaitDelay
	out := &dnsDefaultsBuffer{cancel: cancel}
	cmd.Stdout = out
	// Configuration errors can quote arbitrary non-DNS values. Discard stderr
	// rather than accidentally exposing them through CommandError diagnostics.
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if out.overflow {
		return DNSDefaults{}, errors.New("apple/container DNS defaults exceeded the 64 KiB diagnostic limit")
	}
	if ctx.Err() != nil {
		return DNSDefaults{}, fmt.Errorf("reading apple/container DNS defaults: %w", ctx.Err())
	}
	if err != nil {
		return DNSDefaults{}, errors.New("cannot read apple/container DNS defaults; check that its service is running and system property list --format json is supported")
	}
	return parseDNSDefaults(out.buffer.Bytes())
}

// Bounded while the command runs, not merely after CombinedOutput returns.
// Canceling also stops an unbounded writer that ignores a broken output pipe.
type dnsDefaultsBuffer struct {
	buffer   bytes.Buffer
	cancel   context.CancelFunc
	overflow bool
}

func (b *dnsDefaultsBuffer) Write(p []byte) (int, error) {
	remaining := dnsDefaultsOutputLimit - b.buffer.Len()
	if len(p) <= remaining {
		return b.buffer.Write(p)
	}
	_, _ = b.buffer.Write(p[:remaining])
	b.overflow = true
	b.cancel()
	return remaining, errors.New("DNS defaults output limit exceeded")
}

func parseDNSDefaults(data []byte) (DNSDefaults, error) {
	invalid := errors.New("apple/container returned invalid DNS defaults; unrelated configuration and command output were withheld")
	var config struct {
		Container *struct {
			DNS *struct {
				Nameservers   []string `json:"nameservers"`
				Domain        *string  `json:"domain"`
				SearchDomains []string `json:"searchDomains"`
				Options       []string `json:"options"`
			} `json:"dns"`
		} `json:"container"`
		DNS *struct {
			Domain *string `json:"domain"`
		} `json:"dns"`
	}
	if len(data) > dnsDefaultsOutputLimit || json.Unmarshal(data, &config) != nil || config.Container == nil {
		return DNSDefaults{}, invalid
	}
	var defaults DNSDefaults
	if config.Container.DNS != nil {
		dns := config.Container.DNS
		defaults.Nameservers, defaults.Domain = dns.Nameservers, dns.Domain
		defaults.SearchDomains, defaults.Options = dns.SearchDomains, dns.Options
		if dns.Domain != nil {
			defaults.DomainSource = "container.dns.domain"
		}
	}
	if defaults.Domain == nil && config.DNS != nil && config.DNS.Domain != nil {
		defaults.Domain = config.DNS.Domain
		defaults.DomainSource = "dns.domain fallback"
	}
	// Keep presentation bounded even for a valid but pathological config. These
	// are diagnostic size limits, not an attempt to validate DNS reachability.
	for _, values := range [][]string{defaults.Nameservers, defaults.SearchDomains, defaults.Options} {
		if len(values) > 32 {
			return DNSDefaults{}, invalid
		}
		for _, value := range values {
			if len(value) > 1024 {
				return DNSDefaults{}, invalid
			}
		}
	}
	if defaults.Domain != nil && len(*defaults.Domain) > 1024 {
		return DNSDefaults{}, invalid
	}
	for _, server := range defaults.Nameservers {
		if net.ParseIP(server) == nil {
			return DNSDefaults{}, invalid
		}
	}
	return defaults, nil
}
