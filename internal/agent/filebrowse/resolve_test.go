package filebrowse

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeRel(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", ".", false},
		{".", ".", false},
		{"foo", "foo", false},
		{"foo/bar", "foo/bar", false},
		{"foo/../bar", "", true}, // any ".." component is rejected
		{"../escape", "", true},
		{"foo/../../x", "", true},
		{"/abs", "", true},
		{`foo\bar`, "foo/bar", false},
	}
	for _, tc := range cases {
		got, err := NormalizeRel(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("NormalizeRel(%q): want error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("NormalizeRel(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("NormalizeRel(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateStrategy(t *testing.T) {
	if err := ValidateStrategy("ok-strat"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "..", "a/b", `a\b`, "../x"} {
		if err := ValidateStrategy(bad); err == nil {
			t.Fatalf("ValidateStrategy(%q) should fail", bad)
		}
	}
}

func TestOpenRootRejectsEscapeViaSymlink(t *testing.T) {
	base := t.TempDir()
	strategyDir := filepath.Join(base, "strat")
	if err := os.MkdirAll(filepath.Join(strategyDir, "safe"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(strategyDir, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	root, err := Root(strategyDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	// Opening a symlink that points outside the root must fail with OpenRoot.
	if _, err := root.Open("escape"); err == nil {
		t.Fatal("expected Open of out-of-root symlink to fail")
	}
}

func TestListAndPathTraversal(t *testing.T) {
	base := t.TempDir()
	strategyDir := filepath.Join(base, "strat")
	if err := os.MkdirAll(filepath.Join(strategyDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(strategyDir, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	root, err := Root(strategyDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	listing := List(root, "r1", ".")
	if listing.GetError() != "" {
		t.Fatal(listing.GetError())
	}
	if len(listing.Entries) != 2 {
		t.Fatalf("entries=%d want 2", len(listing.Entries))
	}

	bad := List(root, "r2", "../secret")
	if bad.GetError() == "" {
		t.Fatal("expected traversal error")
	}
}
