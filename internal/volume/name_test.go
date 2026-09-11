package volume

import "testing"

func TestValidateName(t *testing.T) {
	ok := []string{"data", "nats-a-data", "mftik.logs"}
	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v", n, err)
		}
	}
	bad := []string{"", ".", "..", "a/b", `a\b`, "lost+found"}
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) want error", n)
		}
	}
}

func TestValidateMounts(t *testing.T) {
	if err := ValidateMounts([][2]string{{"data", "/var/lib/mftik"}}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMounts([][2]string{{"data", "relative"}}); err == nil {
		t.Fatal("expected relative path error")
	}
	if err := ValidateMounts([][2]string{{"data", "/"}}); err == nil {
		t.Fatal("expected / rejected")
	}
	if err := ValidateMounts([][2]string{
		{"a", "/var/lib/mftik"},
		{"b", "/var/lib/mftik/sub"},
	}); err == nil {
		t.Fatal("expected overlap")
	}
}

func TestShadowsBindSame(t *testing.T) {
	if !ShadowsBindSame("/var/lib/strategon/sts/work", "/var/lib/strategon/sts/work") {
		t.Fatal("equal path should shadow")
	}
	if ShadowsBindSame("/var/lib/mftik", "/var/lib/strategon/sts/work") {
		t.Fatal("unrelated path should not shadow")
	}
}
