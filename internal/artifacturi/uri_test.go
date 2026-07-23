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

func TestParseS3(t *testing.T) {
	ok := []struct {
		in         string
		wantBucket string
		wantKey    string
	}{
		{"s3://artifacts/name/v1/abcd", "artifacts", "name/v1/abcd"},
		{"s3://my-bucket/key", "my-bucket", "key"},
		{"s3://b/a/b/c", "b", "a/b/c"},
	}
	for _, tc := range ok {
		got, err := ParseS3(tc.in)
		if err != nil {
			t.Fatalf("ParseS3(%q): %v", tc.in, err)
		}
		if got.Bucket != tc.wantBucket || got.Key != tc.wantKey {
			t.Fatalf("ParseS3(%q)=%+v, want bucket=%q key=%q", tc.in, got, tc.wantBucket, tc.wantKey)
		}
	}
	bad := []string{
		"",
		"https://example.com/x",
		"s3://",
		"s3://bucket",
		"s3://bucket/",
		"s3:///key",
	}
	for _, uri := range bad {
		if _, err := ParseS3(uri); err == nil {
			t.Errorf("ParseS3(%q) = nil, want error", uri)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := []string{
		"/tmp/x",
		"file:///tmp/x",
		"https://github.com/org/repo/releases/download/v1/strat",
		"http://example.com/bin",
		"s3://artifacts/name/v1/abcd",
		"s3://bucket/key",
	}
	for _, uri := range ok {
		if err := Validate(uri); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", uri, err)
		}
	}
	bad := []string{
		"",
		"file://tmp/x", // two-slash relative
		"s3://",        // no bucket/key
		"s3://bucket",  // no key
		"https://",     // no host
		"tmp/x",        // relative
	}
	for _, uri := range bad {
		if err := Validate(uri); err == nil {
			t.Errorf("Validate(%q) = nil, want error", uri)
		}
	}
}

func TestIsS3(t *testing.T) {
	if !IsS3("s3://b/k") {
		t.Fatal("expected true")
	}
	if IsS3("https://example.com/x") {
		t.Fatal("expected false")
	}
}
