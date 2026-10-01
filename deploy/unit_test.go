package deploy_test

import (
	"os"
	"strings"
	"testing"
)

func TestAgentUnitKillModeProcess(t *testing.T) {
	unit := agentUnit(t)
	if !strings.Contains(unit, "KillMode=process") {
		t.Fatal("unit must set KillMode=process")
	}
	if strings.Contains(unit, "PrivateTmp") {
		t.Fatal("unit must not set PrivateTmp")
	}
	if !strings.Contains(unit, "control-group") {
		t.Fatal("unit comment must warn that the first apply still restarts under control-group")
	}
}

// TestAgentUnitDelegatesCgroup pins the pieces --cgroup-root auto relies on:
// a delegated unit cgroup with memory and cpu, and the agent in a subgroup so
// the unit cgroup can enable controllers for strategies/.
func TestAgentUnitDelegatesCgroup(t *testing.T) {
	unit := agentUnit(t)
	for _, want := range []string{"Delegate=memory cpu pids", "DelegateSubgroup=agent", "--cgroup-root auto"} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit must contain %q", want)
		}
	}
}

func agentUnit(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("install-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	start := strings.Index(text, "render_unit() {")
	if start < 0 {
		t.Fatal("render_unit not found")
	}
	rest := text[start:]
	begin := strings.Index(rest, "cat <<EOF\n")
	end := strings.Index(rest, "\nEOF\n")
	if begin < 0 || end < 0 || end <= begin {
		t.Fatal("unit heredoc not found")
	}
	return rest[begin:end]
}
