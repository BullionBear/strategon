package artifacturi

import "testing"

func TestResolveLocal(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"/tmp/x", "/tmp/x", false},
		{"file:///tmp/x", "/tmp/x", false},
		{"file://localhost/tmp/x", "/tmp/x", false},
		{"file://tmp/x", "", true},
		{"file://tmp/myapp/strat-v1.sh", "", true},
		{"", "", true},
		{"s3://bucket/key", "", true},
		{"tmp/x", "", true},
	}
	for _, tc := range cases {
		got, err := ResolveLocal(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ResolveLocal(%q) err=nil, want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ResolveLocal(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ResolveLocal(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := []string{
		"/tmp/x",
		"file:///tmp/x",
		"https://github.com/org/repo/releases/download/v1/strat",
		"http://example.com/bin",
	}
	for _, uri := range ok {
		if err := Validate(uri); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", uri, err)
		}
	}
	bad := []string{
		"",
		"file://tmp/x",   // two-slash relative
		"s3://bucket/key", // deferred
		"https://",        // no host
		"tmp/x",           // relative
	}
	for _, uri := range bad {
		if err := Validate(uri); err == nil {
			t.Errorf("Validate(%q) = nil, want error", uri)
		}
	}
}
