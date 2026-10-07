package arcade

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fake is the only executable on PATH. These tests cannot fall back to a
// real docker binary and reject all non-inspection commands before doing work.
func installBackendInspectFake(t *testing.T, stdout, stderr, status string) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$ARCADE_INSPECT_CALLS"
if [ "$#" != 6 ] || [ "$1" != inspect ] || [ "$2" != --type ] || [ "$3" != container ] || [ "$4" != -f ] || [ "$5" != '{{.State.Running}}' ] || [ "$6" != "$ARCADE_INSPECT_NAME" ]; then
  printf '%s\n' 'unexpected docker command' >&2
  exit 99
fi
printf '%s' "$ARCADE_INSPECT_STDOUT"
printf '%s' "$ARCADE_INSPECT_STDERR" >&2
exit "$ARCADE_INSPECT_STATUS"
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("ARCADE_INSPECT_CALLS", calls)
	t.Setenv("ARCADE_INSPECT_NAME", containerPrefix+"-backend-probe")
	t.Setenv("ARCADE_INSPECT_STDOUT", stdout)
	t.Setenv("ARCADE_INSPECT_STDERR", stderr)
	t.Setenv("ARCADE_INSPECT_STATUS", status)
	return calls
}

func TestBackendContainerStateCLIClassification(t *testing.T) {
	missing := "Error response from daemon: No such container: " + containerPrefix + "-backend-probe\n"
	for _, tc := range []struct {
		name   string
		stdout string
		stderr string
		status string
		want   ContainerState
	}{
		{"running", "true\n", "", "0", ContainerRunning},
		{"stopped", "false\n", "", "0", ContainerStopped},
		{"missing", "\n", missing, "1", ContainerMissing},
		{"daemon unavailable", "", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n", "1", ContainerUnknown},
		{"permission denied", "", "permission denied while trying to connect to the Docker daemon socket\n", "1", ContainerUnknown},
		{"socket missing", "", "dial unix /var/run/docker.sock: connect: no such file or directory\n", "1", ContainerUnknown},
		{"context missing", "", "context not found\n", "1", ContainerUnknown},
		{"timeout response", "", "context deadline exceeded\n", "1", ContainerUnknown},
		{"malformed success", "unknown\n", "", "0", ContainerUnknown},
		{"empty success", "", "", "0", ContainerUnknown},
		{"multiple results", "true\nfalse\n", "", "0", ContainerUnknown},
		{"partial result and error", "false\n", missing, "1", ContainerUnknown},
		{"wrong name", "", missing + "-other", "1", ContainerUnknown},
		{"mixed errors", "", missing + "permission denied\n", "1", ContainerUnknown},
		{"error with missing substring", "", "transport error: " + missing, "1", ContainerUnknown},
		{"wrong exit status", "", missing, "125", ContainerUnknown},
		{"missing text with success", "", missing, "0", ContainerUnknown},
		{"generic object absence", "", "Error: No such object: " + containerPrefix + "-backend-probe\n", "1", ContainerUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installBackendInspectFake(t, tc.stdout, tc.stderr, tc.status)
			// Call the real production probe, not a replacement state function.
			if got := containerState("backend-probe"); got != tc.want {
				t.Fatalf("containerState = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBackendContainerStateCanceledAndUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{"canceled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}},
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			missing := "Error response from daemon: No such container: " + containerPrefix + "-backend-probe\n"
			calls := installBackendInspectFake(t, "", missing, "1")
			ctx, cancel := tc.ctx()
			defer cancel()
			if got := inspectContainerState(ctx, "backend-probe"); got != ContainerUnknown {
				t.Fatalf("canceled probe = %v, want Unknown", got)
			}
			if _, err := os.Stat(calls); !os.IsNotExist(err) {
				t.Fatalf("canceled context executed docker: %v", err)
			}
		})
	}
	t.Run("binary absent", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if got := containerState("backend-probe"); got != ContainerUnknown {
			t.Fatalf("absent binary = %v, want Unknown", got)
		}
	})
}

func TestBackendUnknownProbeNeverRemovesOrStarts(t *testing.T) {
	for _, stderr := range []string{
		"Cannot connect to the Docker daemon\n",
		"permission denied\n",
		"dial unix /var/run/docker.sock: no such file or directory\n",
		"Error response from daemon: No such container: another-server\n",
	} {
		t.Run(strings.TrimSpace(stderr), func(t *testing.T) {
			calls := installBackendInspectFake(t, "", stderr, "1")
			runner := &dockerRunner{}
			err := runner.Start(&Server{ID: "backend-probe"}, func(Line) {})
			if err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("unknown-state start was not refused: %v", err)
			}
			got, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			want := "inspect --type container -f {{.State.Running}} " + containerPrefix + "-backend-probe\n"
			if string(got) != want {
				t.Fatalf("unexpected command after unknown probe: %q", got)
			}
		})
	}
}
