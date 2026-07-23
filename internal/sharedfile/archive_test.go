package sharedfile

import "testing"

func TestLooksLikeArchive(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"instruments.json", false},
		{"data.csv", false},
		{"instruments.tar.gz", true},
		{"INSTRUMENTS.TAR.GZ", true},
		{"bundle.tgz", true},
		{"x.zip", true},
		{"https://example.com/a/instruments.tar.gz?token=1", true},
		{"file:///var/lib/a/ref.tar", true},
		{"https://example.com/a/instruments.json", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := LooksLikeArchive(tc.in); got != tc.want {
			t.Errorf("LooksLikeArchive(%q)=%v want %v", tc.in, got, tc.want)
		}
	}
}

func TestRejectArchiveRef(t *testing.T) {
	if err := RejectArchiveRef("instruments.json", "catalog", "https://x/y.json"); err != nil {
		t.Fatalf("plain file: %v", err)
	}
	if err := RejectArchiveRef("bundle.tar.gz", "catalog", "https://x/y.json"); err == nil {
		t.Fatal("expected reject on shared name")
	}
	if err := RejectArchiveRef("instruments.json", "bundle.tgz", "https://x/y.json"); err == nil {
		t.Fatal("expected reject on artifact name")
	}
	if err := RejectArchiveRef("instruments.json", "catalog", "https://x/y/data.zip"); err == nil {
		t.Fatal("expected reject on uri")
	}
}
