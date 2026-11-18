package cmd

import (
	"bytes"
	"errors"
	"testing"
)

// runGetKubeconfig runs the command for one cluster name with a stub
// renderer and returns what it printed.
func runGetKubeconfig(t *testing.T, name string, render func(string) (string, error)) (string, error) {
	t.Helper()
	oldName, oldRender := getKubeconfigName, renderKubeconfig
	t.Cleanup(func() {
		getKubeconfigName, renderKubeconfig = oldName, oldRender
		getKubeconfigCmd.SetOut(nil)
	})
	getKubeconfigName, renderKubeconfig = name, render
	var output bytes.Buffer
	getKubeconfigCmd.SetOut(&output)
	err := getKubeconfigCmd.RunE(getKubeconfigCmd, nil)
	return output.String(), err
}

func TestGetKubeconfigPrintsOnlyTheKubeconfig(t *testing.T) {
	const kubeconfig = "apiVersion: v1\nkind: Config\ncurrent-context: kiac-demo\n"
	output, err := runGetKubeconfig(t, "demo", func(name string) (string, error) {
		if name != "demo" {
			t.Fatalf("rendered cluster %q, want demo", name)
		}
		return kubeconfig, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if output != kubeconfig {
		t.Fatalf("output = %q, want exactly the kubeconfig so redirection stays valid", output)
	}
}

func TestGetKubeconfigRejectsInvalidNameBeforeRuntime(t *testing.T) {
	for _, name := range []string{"", "Bad_Name", "dev/../prod"} {
		output, err := runGetKubeconfig(t, name, func(string) (string, error) {
			t.Fatalf("renderer called for invalid name %q", name)
			return "", nil
		})
		if err == nil || output != "" {
			t.Errorf("name %q: error=%v output=%q, want rejection with no output", name, err, output)
		}
	}
}

func TestGetKubeconfigReturnsRendererError(t *testing.T) {
	failure := errors.New(`no cluster named "demo" found`)
	output, err := runGetKubeconfig(t, "demo", func(string) (string, error) { return "", failure })
	if !errors.Is(err, failure) || output != "" {
		t.Fatalf("error=%v output=%q, want renderer error and no output", err, output)
	}
}
