package deploy_test

import (
	"os"
	"strings"
	"testing"
)

func TestAgentUnitKillModeProcess(t *testing.T) {
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
	unit := rest[begin:end]
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
