package cosupload

import "testing"

func TestValidate(t *testing.T) {
	if err := (Options{}).Validate(); err == nil {
		t.Fatal("空 Options 应报错")
	}
	opts := Options{Bucket: "mybucket-1250000000", Region: "ap-guangzhou", SecretID: "AKID", SecretKey: "SECRET"}
	if err := opts.Validate(); err != nil {
		t.Fatalf("合法 Options 应通过: %v", err)
	}
	// 缺 region
	if err := (Options{Bucket: "b", SecretID: "a", SecretKey: "b"}).Validate(); err == nil {
		t.Fatal("缺 region 应报错")
	}
	// 缺凭证
	if err := (Options{Bucket: "b", Region: "ap-guangzhou"}).Validate(); err == nil {
		t.Fatal("缺凭证应报错")
	}
}

func TestObjectKey(t *testing.T) {
	cases := []struct {
		prefix, path, want string
	}{
		{"", "a.tar.gz", "a.tar.gz"},
		{"pixiu-offline/", "a.tar.gz", "pixiu-offline/a.tar.gz"},
		{"pixiu-offline", "a.tar.gz", "pixiu-offline/a.tar.gz"},
		{"/releases/v1.27.3/", "a.tar.gz", "releases/v1.27.3/a.tar.gz"},
	}
	for _, c := range cases {
		if got := ObjectKey(c.prefix, c.path); got != c.want {
			t.Errorf("ObjectKey(%q, %q) = %q, want %q", c.prefix, c.path, got, c.want)
		}
	}
}
