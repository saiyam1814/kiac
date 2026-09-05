package cluster

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// kubeconfigPath returns the file kiac merges contexts into, honoring a
// single-path KUBECONFIG when set.
func kubeconfigPath() (string, error) {
	if env := os.Getenv("KUBECONFIG"); env != "" && !strings.Contains(env, string(os.PathListSeparator)) {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kube", "config"), nil
}

// rewriteAdminConf renames admin.conf's first cluster/user/context to
// kiac-<name> and points the server at the node's reachable IP.
func rewriteAdminConf(name, adminConf, serverIP string) (cluster0, user0, context map[string]any, err error) {
	entry := "kiac-" + name

	var admin map[string]any
	if err := yaml.Unmarshal([]byte(adminConf), &admin); err != nil {
		return nil, nil, nil, fmt.Errorf("parsing admin.conf: %w", err)
	}

	clusters, _ := admin["clusters"].([]any)
	users, _ := admin["users"].([]any)
	if len(clusters) == 0 || len(users) == 0 {
		return nil, nil, nil, fmt.Errorf("admin.conf missing clusters or users")
	}

	cluster0, _ = clusters[0].(map[string]any)
	clusterBody, _ := cluster0["cluster"].(map[string]any)
	cluster0["name"] = entry
	if clusterBody != nil {
		// net.JoinHostPort brackets an IPv6 literal ([fd00::1]:6443) and
		// leaves IPv4 untouched, so the ipv6-only server URL is valid.
		clusterBody["server"] = "https://" + net.JoinHostPort(serverIP, "6443")
	}

	user0, _ = users[0].(map[string]any)
	user0["name"] = entry

	context = map[string]any{
		"name": entry,
		"context": map[string]any{
			"cluster": entry,
			"user":    entry,
		},
	}
	return cluster0, user0, context, nil
}

// writeTempKubeconfig materializes a standalone kubeconfig for host
// tools (like the cilium installer) that need the cluster before its
// context is merged into ~/.kube/config at the end of Create. The
// caller removes the file.
func writeTempKubeconfig(name, adminConf, serverIP string) (string, error) {
	content, err := standaloneKubeconfig(name, adminConf, serverIP)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "kiac-kubeconfig-*")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), f.Close()
}

// standaloneKubeconfig renders a self-contained kubeconfig for one
// cluster, e.g. for download from the UI.
func standaloneKubeconfig(name, adminConf, serverIP string) (string, error) {
	cluster0, user0, context, err := rewriteAdminConf(name, adminConf, serverIP)
	if err != nil {
		return "", err
	}
	out, err := yaml.Marshal(map[string]any{
		"apiVersion":      "v1",
		"kind":            "Config",
		"clusters":        []any{cluster0},
		"users":           []any{user0},
		"contexts":        []any{context},
		"current-context": "kiac-" + name,
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// serviceAccountKubeconfig keeps the cluster CA and reachable API server
// from admin.conf but replaces its administrator client certificate with a
// narrowly scoped service-account token used only by the node edge proxy.
func serviceAccountKubeconfig(name, adminConf, serverIP, token string) (string, error) {
	entry := "kiac-" + name + "-edge-proxy"
	cluster0, _, _, err := rewriteAdminConf(name+"-edge-proxy", adminConf, serverIP)
	if err != nil {
		return "", err
	}
	user := map[string]any{
		"name": entry,
		"user": map[string]any{"token": token},
	}
	context := map[string]any{
		"name": entry,
		"context": map[string]any{
			"cluster": entry,
			"user":    entry,
		},
	}
	out, err := yaml.Marshal(map[string]any{
		"apiVersion":      "v1",
		"kind":            "Config",
		"clusters":        []any{cluster0},
		"users":           []any{user},
		"contexts":        []any{context},
		"current-context": entry,
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// mergeKubeconfig rewrites the cluster's admin.conf to use kiac-<name>
// entry names and the node's reachable IP, merges it into the user's
// kubeconfig, and switches current-context. Returns the merged file path.
func mergeKubeconfig(name, adminConf, serverIP string) (string, error) {
	entry := "kiac-" + name
	cluster0, user0, context, err := rewriteAdminConf(name, adminConf, serverIP)
	if err != nil {
		return "", err
	}

	path, err := kubeconfigPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}

	dest := map[string]any{
		"apiVersion": "v1",
		"kind":       "Config",
	}
	if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &dest); err != nil {
			return "", fmt.Errorf("parsing existing kubeconfig %s: %w", path, err)
		}
		// Keep the FIRST pre-kiac state only: overwriting per create
		// would replace the user's true original with one kiac already
		// touched.
		backup := path + ".kiac.bak"
		if _, err := os.Stat(backup); os.IsNotExist(err) {
			if err := os.WriteFile(backup, raw, 0o600); err != nil {
				return "", fmt.Errorf("writing kubeconfig backup: %w", err)
			}
		}
	}

	dest["clusters"] = upsert(listOf(dest["clusters"]), entry, cluster0)
	dest["users"] = upsert(listOf(dest["users"]), entry, user0)
	dest["contexts"] = upsert(listOf(dest["contexts"]), entry, context)
	dest["current-context"] = entry

	out, err := yaml.Marshal(dest)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// removeKubeconfig strips the cluster's entries and clears
// current-context if it pointed at the deleted cluster.
func removeKubeconfig(name string) error {
	entry := "kiac-" + name
	path, err := kubeconfigPath()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil
	}
	var dest map[string]any
	if err := yaml.Unmarshal(raw, &dest); err != nil {
		return fmt.Errorf("parsing kubeconfig %s: %w", path, err)
	}
	if dest == nil {
		return nil
	}
	dest["clusters"] = drop(listOf(dest["clusters"]), entry)
	dest["users"] = drop(listOf(dest["users"]), entry)
	dest["contexts"] = drop(listOf(dest["contexts"]), entry)
	if cur, _ := dest["current-context"].(string); cur == entry {
		dest["current-context"] = ""
	}
	out, err := yaml.Marshal(dest)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

func listOf(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

func nameOf(item any) string {
	m, ok := item.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["name"].(string)
	return s
}

// upsert replaces the named entry or appends it.
func upsert(list []any, name string, item any) []any {
	for i, existing := range list {
		if nameOf(existing) == name {
			list[i] = item
			return list
		}
	}
	return append(list, item)
}

func drop(list []any, name string) []any {
	out := make([]any, 0, len(list))
	for _, existing := range list {
		if nameOf(existing) != name {
			out = append(out, existing)
		}
	}
	return out
}
