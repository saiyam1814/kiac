package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorRequiresPatchedRuntimeBeforeFixingService(t *testing.T) {
	for _, tc := range []struct {
		name, output, status string
		wantOK, wantStart    bool
	}{
		{name: "vulnerable", output: "container CLI version 1.5.0"},
		{name: "prerelease", output: "container CLI version 1.6.0-rc.1"},
		{name: "unknown", output: "unknown"},
		{name: "failed probe", status: "1"},
		{name: "patched", output: "container CLI version 1.6.0", wantOK: true, wantStart: true},
		{name: "recommended", output: "container CLI version 1.6.1", wantOK: true, wantStart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			for name, script := range map[string]string{
				"container": `#!/bin/sh
printf '%s\n' "$*" >> "$KIAC_TEST_CALLS"
case "$1" in
  --version) printf '%s\n' "$KIAC_TEST_VERSION"; exit "${KIAC_TEST_VERSION_STATUS:-0}" ;;
  system) printf '%s\n' 'running' ;;
esac
`,
				"sw_vers": "#!/bin/sh\nprintf '26.0\\n'\n",
				"uname":   "#!/bin/sh\nprintf 'arm64\\n'\n",
				"kubectl": "#!/bin/sh\nexit 0\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir)
			t.Setenv("KIAC_TEST_CALLS", calls)
			t.Setenv("KIAC_TEST_VERSION", tc.output)
			t.Setenv("KIAC_TEST_VERSION_STATUS", tc.status)
			oldFix := doctorFix
			doctorFix = true
			t.Cleanup(func() { doctorFix = oldFix })
			err := doctorCmd.RunE(doctorCmd, nil)
			if (err == nil) != tc.wantOK {
				t.Fatalf("doctor error = %v, want success=%v", err, tc.wantOK)
			}
			commands, readErr := os.ReadFile(calls)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if started := strings.Contains(string(commands), "system start"); started != tc.wantStart {
				t.Fatalf("service started=%v, want %v; commands:\n%s", started, tc.wantStart, commands)
			}
		})
	}
}

func TestDoctorDNSIsExplicitReadOnlyAndPrivate(t *testing.T) {
	for _, tc := range []struct {
		name, properties, version string
		enabled, fix, wantErr     bool
	}{
		{name: "not requested", properties: "invalid private config DO-NOT-DISCLOSE"},
		{name: "1.6 runtime defaults", enabled: true, version: "1.6.1", properties: `{"container":{"cpus":4},"dns":{"domain":"host.test"},"registry":{"password":"DO-NOT-DISCLOSE"}}`},
		{name: "container fields", enabled: true, properties: `{"container":{"dns":{"nameservers":["1.1.1.1"],"domain":"nodes.test","searchDomains":["corp.test"],"options":["ndots:2"]}},"dns":{"domain":"host.test"},"registry":{"password":"DO-NOT-DISCLOSE"}}`},
		{name: "fix happens before requested DNS query", enabled: true, fix: true, properties: `{"container":{},"dns":{}}`},
		{name: "requested malformed config", enabled: true, wantErr: true, properties: "invalid private config DO-NOT-DISCLOSE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			for name, script := range map[string]string{
				"container": `#!/bin/sh
printf '%s\n' "$*" >> "$KIAC_TEST_CALLS"
case "$*" in
  --version) printf 'container CLI version %s\n' "$KIAC_TEST_VERSION" ;;
  'system status')
    if test "$KIAC_TEST_EXPECT_START" = 1 && ! test -f "$KIAC_TEST_STARTED"; then printf '%s\n' 'not running'; else printf '%s\n' 'running'; fi ;;
  'system start --enable-kernel-install') : > "$KIAC_TEST_STARTED" ;;
  'system property list --format json')
    if test "$KIAC_TEST_EXPECT_START" = 1; then test -f "$KIAC_TEST_STARTED" || exit 42; fi
    printf '%s\n' "$KIAC_TEST_PROPERTIES"; printf '%s\n' 'private stderr DO-NOT-DISCLOSE' >&2 ;;
  *) exit 42 ;;
esac
`,
				"sw_vers": "#!/bin/sh\nprintf '26.0\\n'\n",
				"uname":   "#!/bin/sh\nprintf 'arm64\\n'\n",
				"kubectl": "#!/bin/sh\nexit 0\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir)
			t.Setenv("KIAC_TEST_CALLS", calls)
			t.Setenv("KIAC_TEST_PROPERTIES", tc.properties)
			version := tc.version
			if version == "" {
				version = "1.7.1"
			}
			t.Setenv("KIAC_TEST_VERSION", version)
			t.Setenv("KIAC_TEST_STARTED", filepath.Join(dir, "started"))
			expectStart := "0"
			if tc.fix {
				expectStart = "1"
			}
			t.Setenv("KIAC_TEST_EXPECT_START", expectStart)
			oldDNS, oldFix := doctorDNS, doctorFix
			doctorDNS, doctorFix = tc.enabled, tc.fix
			t.Cleanup(func() { doctorDNS, doctorFix = oldDNS, oldFix })
			out, err := os.CreateTemp(dir, "stdout")
			if err != nil {
				t.Fatal(err)
			}
			oldStdout := os.Stdout
			os.Stdout = out
			t.Cleanup(func() { os.Stdout = oldStdout })
			err = doctorCmd.RunE(doctorCmd, nil)
			os.Stdout = oldStdout
			if closeErr := out.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("doctor error = %v, want error=%v", err, tc.wantErr)
			}
			output, readErr := os.ReadFile(out.Name())
			if readErr != nil {
				t.Fatal(readErr)
			}
			commands, readErr := os.ReadFile(calls)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if queried := strings.Contains(string(commands), "system property list --format json"); queried != tc.enabled {
				t.Fatalf("DNS query=%v, requested=%v; calls=%s", queried, tc.enabled, commands)
			}
			if started := strings.Contains(string(commands), "system start"); started != tc.fix {
				t.Fatalf("service started=%v, --fix=%v; calls=%s", started, tc.fix, commands)
			}
			if strings.Contains(string(output), "DO-NOT-DISCLOSE") {
				t.Fatalf("doctor leaked unrelated configuration; output=%s", output)
			}
			if tc.enabled && !tc.wantErr {
				for _, expected := range []string{"nameservers:", "domain:", "search domains:", "resolver options:", "replaces nameservers only"} {
					if !strings.Contains(string(output), expected) {
						t.Errorf("missing %q from %s", expected, output)
					}
				}
			}
		})
	}
}
