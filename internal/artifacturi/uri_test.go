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
