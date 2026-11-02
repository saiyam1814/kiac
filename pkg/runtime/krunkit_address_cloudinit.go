package runtime

import "fmt"

func guestAddressCloudInitFiles(state KrunkitNodeState) []cloudInitWriteFile {
	return []cloudInitWriteFile{
		{Path: "/etc/systemd/system/run-kiac-status.mount", Permissions: "0644", Content: `[Unit]
Description=Kiac private guest status share
Before=kiac-network-report.service

[Mount]
What=kiac-status
Where=/run/kiac/status
Type=virtiofs
Options=rw,nosuid,nodev,noexec
TimeoutSec=10s

[Install]
WantedBy=multi-user.target
`},
		{Path: "/etc/systemd/system/kiac-network-report.service", Permissions: "0644", Content: `[Unit]
Description=Report this VM's address to Kiac
Wants=network-online.target
After=network-online.target run-kiac-status.mount
Requires=run-kiac-status.mount

[Service]
Type=oneshot
ExecStart=/usr/local/libexec/kiac-network-report
TimeoutStartSec=10s
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/run/kiac/status
`},
		{Path: "/etc/systemd/system/kiac-network-report.timer", Permissions: "0644", Content: `[Unit]
Description=Refresh Kiac guest address report

[Timer]
OnBootSec=1s
OnUnitActiveSec=3s
AccuracySec=1s

[Install]
WantedBy=timers.target
`},
		{Path: "/usr/local/libexec/kiac-network-report", Permissions: "0755", Content: guestAddressReporter(state)},
	}
}

// Python is already part of the pinned Fedora cloud-init image. The reporter
// uses only its standard library and writes one small atomic JSON record.
func guestAddressReporter(state KrunkitNodeState) string {
	return fmt.Sprintf(`#!/usr/bin/python3
import ipaddress
import json
import os
import re
import subprocess
import tempfile

share = "/run/kiac/status"
with open(share + "/generation.json") as source:
    generation = json.loads(source.read(128))
if not isinstance(generation, str) or not re.fullmatch("[0-9a-f]{32}", generation):
    raise ValueError("invalid boot generation")
with open("/sys/class/net/eth0/address") as source:
    mac = source.read(64).strip()
network = ipaddress.ip_network(%q)
interfaces = json.loads(subprocess.check_output(["ip", "-4", "-j", "address", "show", "dev", "eth0"], timeout=3))
addresses = [item["local"] for interface in interfaces for item in interface.get("addr_info", []) if item.get("scope") == "global"]
address = next((value for value in addresses if ipaddress.ip_address(value) in network and 2 <= int(value.rsplit(".", 1)[1]) <= 200), None)
if address is None:
    raise ValueError("DHCP address is not ready")
report = {"schema": 1, "name": %q, "mac": mac, "generation": generation, "ipv4": address}
filename = None
try:
    with tempfile.NamedTemporaryFile(mode="w", dir=share, prefix=".address-", delete=False) as target:
        filename = target.name
        json.dump(report, target)
        target.write("\n")
    os.replace(filename, share + "/address.json")
finally:
    if filename is not None and os.path.exists(filename):
        os.unlink(filename)
`, state.NetworkCIDR, state.Name)
}
