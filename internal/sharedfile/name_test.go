package sharedfile

import "testing"

func TestValidateName(t *testing.T) {
	ok := []string{"instruments.json", "foo", "a-b_c.1"}
	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	bad := []string{"", "store", ".", "..", "a/b", `a\b`, "x\x00y"}
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("%q: expected error", n)
		}
	}
}
