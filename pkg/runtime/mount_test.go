package runtime

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseMount(t *testing.T) {
	mount, err := ParseMount("type=bind,source=/Users/me/project files,target=/workspace,readonly")
	if err != nil {
		t.Fatal(err)
	}
	want := Mount{Source: "/Users/me/project files", Target: "/workspace", ReadOnly: true}
	if !reflect.DeepEqual(mount, want) {
		t.Fatalf("mount = %+v, want %+v", mount, want)
	}
}

func TestMountsSetAppends(t *testing.T) {
	var mounts Mounts
	for _, value := range []string{
		"type=bind,source=/one,target=/workspace",
		"type=bind,source=/two,target=/data,readonly",
	} {
		if err := mounts.Set(value); err != nil {
			t.Fatal(err)
		}
	}
	if len(mounts) != 2 || mounts[0].Target != "/workspace" || !mounts[1].ReadOnly {
		t.Fatalf("mounts = %+v", mounts)
	}
}

func TestParseMountRejectsInvalidSyntax(t *testing.T) {
	for _, tc := range []struct {
		name, value, want string
	}{
		{"missing type", "source=/host,target=/node", "type=bind is required"},
		{"wrong type", "type=volume,source=/host,target=/node", "type must be bind"},
		{"missing source", "type=bind,target=/node", "source is required"},
		{"missing target", "type=bind,source=/host", "target is required"},
		{"unknown field", "type=bind,source=/host,target=/node,bind-propagation=rshared", "unknown field"},
		{"readonly value", "type=bind,source=/host,target=/node,readonly=true", "does not take a value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseMount(tc.value)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestValidateMounts(t *testing.T) {
	source := t.TempDir()
	file := filepath.Join(source, "file")
	if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(source, "missing")

	for _, tc := range []struct {
		name   string
		mounts []Mount
		want   string
	}{
		{"valid", []Mount{{Source: source, Target: "/workspace"}}, ""},
		{"relative source", []Mount{{Source: "relative", Target: "/workspace"}}, "absolute host path"},
		{"missing source", []Mount{{Source: missing, Target: "/workspace"}}, "source is not accessible"},
		{"file source", []Mount{{Source: file, Target: "/workspace"}}, "source must be a directory"},
		{"relative target", []Mount{{Source: source, Target: "workspace"}}, "absolute Linux path"},
		{"duplicate target", []Mount{{Source: source, Target: "/workspace"}, {Source: source, Target: "/workspace/"}}, "duplicates mount 1"},
		{"comma in source", []Mount{{Source: source + ",data", Target: "/workspace"}}, "commas are not supported"},
		{"comma in target", []Mount{{Source: source, Target: "/work,space"}}, "commas are not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMounts(tc.mounts)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
