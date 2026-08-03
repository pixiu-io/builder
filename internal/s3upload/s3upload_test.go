package s3upload

import "testing"

func TestObjectKey(t *testing.T) {
	cases := []struct {
		prefix, path, want string
	}{
		{"", "/tmp/a.tar.gz", "a.tar.gz"},
		{"pixiu-offline/", "/tmp/a.tar.gz", "pixiu-offline/a.tar.gz"},
		{"pixiu-offline", "/tmp/a.tar.gz", "pixiu-offline/a.tar.gz"},
		{"/releases/v1/", "b.tar.gz", "releases/v1/b.tar.gz"},
		{"  ", "c.tar.gz", "c.tar.gz"},
	}
	for _, c := range cases {
		if got := ObjectKey(c.prefix, c.path); got != c.want {
			t.Errorf("ObjectKey(%q, %q) = %q, want %q", c.prefix, c.path, got, c.want)
		}
	}
}

func TestOptionsValidate(t *testing.T) {
	if err := (Options{}).Validate(); err == nil {
		t.Fatal("空 bucket 应报错")
	}
	if err := (Options{Bucket: "b"}).Validate(); err != nil {
		t.Fatalf("有 bucket 应通过: %v", err)
	}
}

func TestFormatURI(t *testing.T) {
	got := formatURI(Options{Bucket: "b"}, "k/a.tar.gz")
	if got != "s3://b/k/a.tar.gz" {
		t.Errorf("URI = %q", got)
	}
	got = formatURI(Options{Bucket: "b", Endpoint: "http://127.0.0.1:9000/"}, "k/a.tar.gz")
	if got != "http://127.0.0.1:9000/b/k/a.tar.gz" {
		t.Errorf("custom URI = %q", got)
	}
}
