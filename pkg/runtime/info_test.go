package runtime

import "testing"

// 0.x shape: top-level status string and networks array with "address".
const lsV0IP = `[{"status":"running","networks":[{"address":"192.168.64.2/24"}],"configuration":{"id":"kiac-dev-control-plane","creationDate":"2026-06-11T10:00:00Z","image":{"reference":"docker.io/kindest/node:v1.34.0"}}}]`

// 1.x shape: status object with state and networks with "ipv4Address".
const lsV1IP = `[{"id":"kiac-dev-worker-1","configuration":{"id":"kiac-dev-worker-1","creationDate":"2026-06-11T10:00:05Z","image":{"reference":"docker.io/kindest/node:v1.36.1"}},"status":{"state":"running","networks":[{"ipv4Address":"192.168.65.13/24"}]}}]`

// Stopped 1.x container: empty networks, no address to report.
const lsV1Stopped = `[{"id":"kiac-dev-worker-2","configuration":{"id":"kiac-dev-worker-2","creationDate":"2026-06-11T10:00:07Z","image":{"reference":"docker.io/kindest/node:v1.36.1"}},"status":{"state":"stopped","networks":[]}}]`

func TestParseListIPAndCreated(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		ip      string
		created string
		status  string
	}{
		{name: "v0", in: lsV0IP, ip: "192.168.64.2", created: "2026-06-11T10:00:00Z", status: "running"},
		{name: "v1", in: lsV1IP, ip: "192.168.65.13", created: "2026-06-11T10:00:05Z", status: "running"},
		{name: "v1 stopped", in: lsV1Stopped, ip: "", created: "2026-06-11T10:00:07Z", status: "stopped"},
	}
	for _, c := range cases {
		infos, err := parseList(c.in, "kiac-dev-")
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(infos) != 1 {
			t.Errorf("%s: expected 1 row, got %+v", c.name, infos)
			continue
		}
		i := infos[0]
		if i.IP != c.ip || i.Created != c.created || i.Status != c.status {
			t.Errorf("%s: got IP=%q Created=%q Status=%q, want IP=%q Created=%q Status=%q",
				c.name, i.IP, i.Created, i.Status, c.ip, c.created, c.status)
		}
	}
}
