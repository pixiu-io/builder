package mirror

import "testing"

func TestParseMirror(t *testing.T) {
	cases := []struct {
		in   string
		want Mirror
		ok   bool
	}{
		{"official", Official, true},
		{"OFFICIAL", Official, true},
		{"aliyun", Aliyun, true},
		{"tencent", Tencent, true},
		{"mirror.aliyuncs.com", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, err := ParseMirror(c.in)
		if c.ok && err != nil {
			t.Errorf("ParseMirror(%q) 意外错误: %v", c.in, err)
			continue
		}
		if !c.ok && err == nil {
			t.Errorf("ParseMirror(%q) 期望错误，实际 %v", c.in, got)
			continue
		}
		if c.ok && got != c.want {
			t.Errorf("ParseMirror(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIsSupported(t *testing.T) {
	for _, m := range []Mirror{Official, Aliyun, Tencent} {
		if !m.IsSupported() {
			t.Errorf("%s 应标记为已支持", m)
		}
	}
}

func TestImageRepository(t *testing.T) {
	if got := Official.ImageRepository(); got != "registry.k8s.io" {
		t.Errorf("official 仓库 = %q, want registry.k8s.io", got)
	}
	if got := Aliyun.ImageRepository(); got != "registry.aliyuncs.com/google_containers" {
		t.Errorf("aliyun 仓库 = %q", got)
	}
	if got := Tencent.ImageRepository(); got != "mirror.cc.tencentyun.com/kubernetes" {
		t.Errorf("tencent 仓库 = %q", got)
	}
}
