package main

import (
	"strings"
	"testing"
)

func TestLatestSemverTag(t *testing.T) {
	cases := []struct {
		name    string
		tags    []string
		want    string
		wantErr bool
	}{
		{
			name: "实际远端 tag 集合取 v2.0.1（正式版大于 prerelease）",
			tags: []string{"v2.0.1", "v2.0.1-beta.1", "v2.0.1-beta.2", "v2.0.1-beta.3", "v2.0.1-beta.4", "v2.0.1-beta.5"},
			want: "v2.0.1",
		},
		{
			name: "更高核心段的 prerelease 大于低版本正式版",
			tags: []string{"v2.0.1", "v2.0.2-beta.1"},
			want: "v2.0.2-beta.1",
		},
		{
			name: "prerelease 数字标识符按数值比较（beta.10 > beta.5，非字典序）",
			tags: []string{"v2.0.1-beta.5", "v2.0.1-beta.10"},
			want: "v2.0.1-beta.10",
		},
		{
			name: "核心段数字按数值比较（v1.10 > v1.9，非字典序）",
			tags: []string{"v1.9", "v1.10"},
			want: "v1.10",
		},
		{
			name: "prerelease 非数字标识符按字典序（beta > alpha）",
			tags: []string{"v2.0.1-alpha.1", "v2.0.1-beta.1"},
			want: "v2.0.1-beta.1",
		},
		{
			name: "prerelease 前缀相同时标识符多者大（beta.1 > beta）",
			tags: []string{"v2.0.1-beta", "v2.0.1-beta.1"},
			want: "v2.0.1-beta.1",
		},
		{
			name: "prerelease 数字标识符小于非数字标识符（beta < 5）",
			tags: []string{"v2.0.1-beta", "v2.0.1-5"},
			want: "v2.0.1-beta",
		},
		{
			name: "核心段缺失按 0 补齐（v1.0 与 v1 视为相等，字典序取 v1.0）",
			tags: []string{"v1", "v1.0"},
			want: "v1.0",
		},
		{
			name: "v 前缀可省略",
			tags: []string{"1.2.3", "v1.3.0"},
			want: "v1.3.0",
		},
		{
			name: "非法 tag 跳过",
			tags: []string{"foo", "release-1.0", "latest", "v2.0.1", "v2.0.1-beta.1"},
			want: "v2.0.1",
		},
		{
			name: "tie 并列时字典序稳定取定（v2.0.1 > 2.0.1）",
			tags: []string{"2.0.1", "v2.0.1"},
			want: "v2.0.1",
		},
		{
			name: "tie 并列时字典序稳定取定（与输入顺序无关）",
			tags: []string{"v2.0.1", "2.0.1"},
			want: "v2.0.1",
		},
		{
			name:    "全部非法 tag 报错",
			tags:    []string{"foo", "bar", "1.x", "v"},
			wantErr: true,
		},
		{
			name:    "空列表报错",
			tags:    nil,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		got, err := latestSemverTag(tc.tags)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: latestSemverTag(%v) err=nil, want error", tc.name, tc.tags)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: latestSemverTag(%v) err=%v", tc.name, tc.tags, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: latestSemverTag(%v)=%q, want %q", tc.name, tc.tags, got, tc.want)
		}
	}
}

func TestResolvePixiuVersion(t *testing.T) {
	remoteTags := []string{"v2.0.1", "v2.0.1-beta.1", "v2.0.1-beta.2", "v2.0.1-beta.3", "v2.0.1-beta.4", "v2.0.1-beta.5"}
	cases := []struct {
		name    string
		tags    []string
		ref     string
		want    string
		wantErr bool
	}{
		{
			name: "ref 是分支（master）时取 semver 最大 tag",
			tags: remoteTags,
			ref:  "master",
			want: "v2.0.1",
		},
		{
			name: "ref 本身是 tag 时直接使用（不取最大）",
			tags: remoteTags,
			ref:  "v2.0.1-beta.3",
			want: "v2.0.1-beta.3",
		},
		{
			name: "ref 是非法 semver 但确为 tag 时同样直接使用",
			tags: []string{"latest", "v2.0.1"},
			ref:  "latest",
			want: "latest",
		},
		{
			name: "ref 前后空白被裁剪",
			tags: remoteTags,
			ref:  "  v2.0.1  ",
			want: "v2.0.1",
		},
		{
			name:    "远端无 tag 且 ref 非 tag 时报错",
			tags:    nil,
			ref:     "master",
			wantErr: true,
		},
		{
			name:    "远端 tag 全非法且 ref 非 tag 时报错",
			tags:    []string{"foo", "bar"},
			ref:     "master",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		got, err := resolvePixiuVersion(tc.tags, tc.ref)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: resolvePixiuVersion(%v, %q) err=nil, want error", tc.name, tc.tags, tc.ref)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: resolvePixiuVersion(%v, %q) err=%v", tc.name, tc.tags, tc.ref, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: resolvePixiuVersion(%v, %q)=%q, want %q", tc.name, tc.tags, tc.ref, got, tc.want)
		}
	}
}

func TestParseLsRemoteTags(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "典型 ls-remote 输出（tab 分隔；跳过 HEAD 与 peeled 行）",
			in: "c8afd7e18bfbb508006ed27e84662f9ec1d09157\tHEAD\n" +
				"c8afd7e18bfbb508006ed27e84662f9ec1d09157\trefs/tags/v2.0.1\n" +
				"77b74dea60b420f04607bad50e2f2ec1ab5260ae\trefs/tags/v2.0.1-beta.1\n" +
				"77b74dea60b420f04607bad50e2f2ec1ab5260ae\trefs/tags/v2.0.1-beta.1^{}\n",
			want: []string{"v2.0.1", "v2.0.1-beta.1"},
		},
		{
			name: "空输出",
			in:   "",
			want: nil,
		},
		{
			name: "残缺行与空行被跳过",
			in:   "\n  \ngarbage\nc8afd7e18bfbb508006ed27e84662f9ec1d09157\n",
			want: nil,
		},
	}
	for _, tc := range cases {
		got := parseLsRemoteTags(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("%s: parseLsRemoteTags=%v, want %v", tc.name, got, tc.want)
			continue
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("%s: parseLsRemoteTags[%d]=%q, want %q", tc.name, i, got[i], tc.want[i])
			}
		}
	}
}

func TestDeployAgentGitHubTag(t *testing.T) {
	old := githubTag
	t.Cleanup(func() { githubTag = old })

	githubTag = ""
	if got := deployAgentGitHubTag("v2.0.1"); got != "deploy-agent-v2.0.1" {
		t.Fatalf("default tag = %q, want deploy-agent-v2.0.1", got)
	}
	if got := deployAgentGitHubTag(""); got != "deploy-agent-" {
		t.Fatalf("empty version tag = %q, want deploy-agent-", got)
	}

	githubTag = "  deploy-agent  "
	if got := deployAgentGitHubTag("v2.0.1"); got != "deploy-agent" {
		t.Fatalf("override tag = %q, want deploy-agent", got)
	}
}

func TestDeployAgentAssetName(t *testing.T) {
	if got := deployAgentAssetName("v2.0.1", "amd64"); got != "pixiu-deploy-agent-v2.0.1-amd64" {
		t.Fatalf("got %q, want pixiu-deploy-agent-v2.0.1-amd64", got)
	}
	if got := deployAgentAssetName("v2.0.1", "arm64"); got != "pixiu-deploy-agent-v2.0.1-arm64" {
		t.Fatalf("got %q, want pixiu-deploy-agent-v2.0.1-arm64", got)
	}
	if strings.Contains(deployAgentAssetName("v2.0.1", "amd64"), ".tar.gz") {
		t.Fatal("产物应为裸二进制，不应含 .tar.gz 后缀")
	}
}
