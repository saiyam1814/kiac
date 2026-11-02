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
