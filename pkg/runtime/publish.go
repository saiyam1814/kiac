package runtime

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Publish is one container run --publish mapping:
// [host-ip:]host-port:container-port[/protocol].
type Publish struct {
	HostIP        string
	HostPort      int
	ContainerPort int
	Protocol      string // tcp (default) or udp
}

// String renders the value accepted by Apple's container CLI.
func (p Publish) String() string {
	spec := strconv.Itoa(p.HostPort) + ":" + strconv.Itoa(p.ContainerPort)
	if p.HostIP != "" {
		if strings.Contains(p.HostIP, ":") {
			spec = "[" + p.HostIP + "]:" + spec
		} else {
			spec = p.HostIP + ":" + spec
		}
	}
	if p.Protocol != "" && p.Protocol != "tcp" {
		spec += "/" + p.Protocol
	}
	return spec
}

// Publishes is a repeatable pflag.Value for --publish/-p.
type Publishes []string

func (p *Publishes) Set(value string) error {
	mapping, err := ParsePublish(value)
	if err != nil {
		return err
	}
	*p = append(*p, mapping.String())
	return nil
}

func (p *Publishes) String() string {
	if p == nil {
		return ""
	}
	return strings.Join(*p, ";")
}

func (*Publishes) Type() string { return "publish" }

// ParsePublish parses [host-ip:]host-port:container-port[/protocol].
func ParsePublish(value string) (Publish, error) {
	spec := strings.TrimSpace(value)
	if spec == "" {
		return Publish{}, fmt.Errorf("invalid --publish %q: value is required", value)
	}
	protocol := "tcp"
	if slash := strings.LastIndex(spec, "/"); slash >= 0 {
		protocol = strings.ToLower(strings.TrimSpace(spec[slash+1:]))
		spec = strings.TrimSpace(spec[:slash])
		if protocol != "tcp" && protocol != "udp" {
			return Publish{}, fmt.Errorf("invalid --publish %q: protocol must be tcp or udp", value)
		}
	}
	containerSep := lastColonOutsideBrackets(spec)
	if containerSep <= 0 || containerSep >= len(spec)-1 {
		return Publish{}, fmt.Errorf("invalid --publish %q: expected [host-ip:]host-port:container-port[/protocol]", value)
	}
	left, containerPortRaw := spec[:containerSep], spec[containerSep+1:]
	hostSep := lastColonOutsideBrackets(left)
	hostIPRaw, hostPortRaw := "", left
	if hostSep >= 0 {
		hostIPRaw, hostPortRaw = left[:hostSep], left[hostSep+1:]
	}
	hostPort, err := parsePort(hostPortRaw)
	if err != nil {
		return Publish{}, fmt.Errorf("invalid --publish %q: %w", value, err)
	}
	containerPort, err := parsePort(containerPortRaw)
	if err != nil {
		return Publish{}, fmt.Errorf("invalid --publish %q: %w", value, err)
	}
	hostIP, err := parseHostIP(hostIPRaw)
	if err != nil {
		return Publish{}, fmt.Errorf("invalid --publish %q: %w", value, err)
	}
	return Publish{
		HostIP:        hostIP,
		HostPort:      hostPort,
		ContainerPort: containerPort,
		Protocol:      protocol,
	}, nil
}

// ValidatePublishes rejects malformed --publish mappings.
func ValidatePublishes(specs []string) error {
	for i, spec := range specs {
		if _, err := ParsePublish(spec); err != nil {
			return fmt.Errorf("publish %d: %w", i+1, err)
		}
	}
	return nil
}

func lastColonOutsideBrackets(s string) int {
	depth := 0
	for i := len(s) - 1; i >= 0; i-- {
		switch s[i] {
		case ']':
			depth++
		case '[':
			if depth > 0 {
				depth--
			}
		case ':':
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func parsePort(raw string) (int, error) {
	port, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("port %q must be an integer in 1..65535", raw)
	}
	return port, nil
}

func parseHostIP(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	addrRaw := strings.TrimSpace(raw)
	if strings.Count(addrRaw, "[") > 1 || strings.Count(addrRaw, "]") > 1 {
		return "", fmt.Errorf("host IP %q has invalid brackets", raw)
	}
	if strings.HasPrefix(addrRaw, "[") || strings.HasSuffix(addrRaw, "]") {
		if !strings.HasPrefix(addrRaw, "[") || !strings.HasSuffix(addrRaw, "]") {
			return "", fmt.Errorf("host IPv6 %q must use [addr] bracket form", raw)
		}
		addrRaw = strings.TrimSpace(addrRaw[1 : len(addrRaw)-1])
		if addrRaw == "" {
			return "", fmt.Errorf("host IP is empty")
		}
	} else if strings.Contains(addrRaw, ":") {
		return "", fmt.Errorf("host IPv6 %q must be bracketed like [::1]", raw)
	}
	addr, err := netip.ParseAddr(addrRaw)
	if err != nil {
		return "", fmt.Errorf("host IP %q is invalid", raw)
	}
	return addr.Unmap().String(), nil
}
