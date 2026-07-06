package cluster

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/saiyam1814/kiac/pkg/runtime"
)

// NodeStatus is one node VM's state as reported by the container runtime.
type NodeStatus struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	IP      string `json:"ip,omitempty"`
	Image   string `json:"image"`
	Created string `json:"created,omitempty"`
}

// ClusterStatus aggregates the node VMs of one cluster. After a host
// reboot the container service forgets running VMs, so Running vs Total
// is the signal that a cluster's VMs are gone, not just idle.
type ClusterStatus struct {
	Name       string       `json:"name"`
	Status     string       `json:"status"`
	Running    int          `json:"runningNodes"`
	Total      int          `json:"totalNodes"`
	K8sVersion string       `json:"k8sVersion,omitempty"`
	Created    string       `json:"created,omitempty"`
	Nodes      []NodeStatus `json:"nodes"`
}

// Statuses lists all kiac clusters with per-node VM state.
func (m *Manager) Statuses() ([]ClusterStatus, error) {
	infos, err := m.rt.List("kiac-")
	if err != nil {
		return nil, err
	}
	return BuildStatuses(infos), nil
}

// BuildStatuses groups raw container rows into per-cluster status,
// sorted by cluster name. Rows outside kiac's node naming are ignored.
func BuildStatuses(infos []runtime.Info) []ClusterStatus {
	byName := map[string]*ClusterStatus{}
	for _, i := range infos {
		name, ok := clusterNameFromNode(i.Name)
		if !ok {
			continue
		}
		cs := byName[name]
		if cs == nil {
			cs = &ClusterStatus{Name: name}
			byName[name] = cs
		}
		cs.Nodes = append(cs.Nodes, NodeStatus{
			Name:    i.Name,
			State:   i.Status,
			IP:      i.IP,
			Image:   i.Image,
			Created: i.Created,
		})
		cs.Total++
		if i.Status == "running" {
			cs.Running++
		}
		if cs.K8sVersion == "" {
			cs.K8sVersion = K8sVersionFromImage(i.Image)
		}
		// The control plane's timestamp is the cluster's; workers only
		// stand in when the control-plane row lacks one.
		if i.Created != "" && (cs.Created == "" || strings.HasSuffix(i.Name, "-control-plane")) {
			cs.Created = i.Created
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ClusterStatus, 0, len(names))
	for _, n := range names {
		cs := byName[n]
		cs.Status = statusText(cs.Running, cs.Total)
		out = append(out, *cs)
	}
	return out
}

// statusText renders running/total in the shape users scan for:
// "3/3 running" is healthy, "0/3 stopped" means the VMs are gone
// (typically after a reboot) and the cluster needs attention.
func statusText(running, total int) string {
	if total > 0 && running == 0 {
		return fmt.Sprintf("0/%d stopped", total)
	}
	return fmt.Sprintf("%d/%d running", running, total)
}

// clusterNameFromNode extracts the cluster name from a node container
// name ("kiac-dev-worker-1" -> "dev"); mirrors prefix()/ControlPlane().
func clusterNameFromNode(node string) (string, bool) {
	rest := strings.TrimPrefix(node, "kiac-")
	if rest == node {
		return "", false
	}
	if idx := strings.LastIndex(rest, "-control-plane"); idx > 0 {
		return rest[:idx], true
	}
	if idx := strings.LastIndex(rest, "-worker-"); idx > 0 {
		return rest[:idx], true
	}
	return "", false
}

var versionTagRe = regexp.MustCompile(`^v?\d+\.\d+`)

// K8sVersionFromImage parses the Kubernetes version out of a node image
// tag ("kindest/node:v1.36.1@sha256:..." -> "v1.36.1"). The runtime
// reports resolved digest-only references ("kindest/node@sha256:...")
// with no tag to parse, so those reverse-map through kiac's own pinned
// image table. Returns "" when neither yields a version.
func K8sVersionFromImage(image string) string {
	tagged := image
	if idx := strings.Index(tagged, "@"); idx >= 0 {
		tagged = tagged[:idx]
	}
	if idx := strings.LastIndex(tagged, ":"); idx >= 0 {
		if tag := tagged[idx+1:]; versionTagRe.MatchString(tag) {
			return tag
		}
	}
	if idx := strings.Index(image, "@sha256:"); idx >= 0 {
		digest := image[idx+1:]
		for _, pinned := range nodeImages {
			if strings.HasSuffix(pinned, digest) {
				return K8sVersionFromImage(strings.Split(pinned, "@")[0])
			}
		}
	}
	return ""
}

// FormatCreated renders a runtime creation timestamp for table output;
// values that are not RFC3339 pass through untouched.
func FormatCreated(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04")
}
