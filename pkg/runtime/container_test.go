package runtime

import "testing"

// container CLI 0.x ls --format json shape (nested configuration object).
const lsV0 = `[{"status":"running","networks":[{"address":"192.168.64.2/24"}],"configuration":{"id":"kiac-dev-control-plane","image":{"reference":"docker.io/kindest/node:v1.34.0"}}},{"status":"running","configuration":{"id":"unrelated","image":{"reference":"nginx"}}}]`

// container CLI 1.x flatter shape.
const lsV1 = `[{"id":"kiac-dev-worker-1","image":"docker.io/kindest/node:v1.34.0","status":"stopped"}]`

func TestParseListShapes(t *testing.T) {
	infos, err := parseList(lsV0, "kiac-dev-")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Name != "kiac-dev-control-plane" || infos[0].Status != "running" {
		t.Errorf("v0 shape parsed wrong: %+v", infos)
	}

	infos, err = parseList(lsV1, "kiac-dev-")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Name != "kiac-dev-worker-1" || infos[0].Status != "stopped" {
		t.Errorf("v1 shape parsed wrong: %+v", infos)
	}
}
