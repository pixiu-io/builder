package main

import (
	"testing"
)

func TestAuthedRepoURL(t *testing.T) {
	oldToken, oldRepoToken := githubToken, gitRepoToken
	t.Cleanup(func() { githubToken, gitRepoToken = oldToken, oldRepoToken })

	cases := []struct {
		token, repoToken, in, want string
	}{
		// 无 token：原样返回
		{"", "", "https://github.com/caoyingjunz/rainbow.git", "https://github.com/caoyingjunz/rainbow.git"},
		// 仅有 --github-token：注入它
		{"tok", "", "https://github.com/caoyingjunz/rainbow.git", "https://x-access-token:tok@github.com/caoyingjunz/rainbow.git"},
		// --repo-token 优先于 --github-token
		{"tok", "rtok", "https://github.com/caoyingjunz/rainbow.git", "https://x-access-token:rtok@github.com/caoyingjunz/rainbow.git"},
		// 已含凭据：不改写
		{"tok", "", "https://user:pass@github.com/caoyingjunz/rainbow.git", "https://user:pass@github.com/caoyingjunz/rainbow.git"},
		// 非 GitHub URL：不改写
		{"tok", "", "/Users/dev/rainbow", "/Users/dev/rainbow"},
		{"tok", "", "https://gitlab.com/foo/bar.git", "https://gitlab.com/foo/bar.git"},
	}
	for _, c := range cases {
		githubToken, gitRepoToken = c.token, c.repoToken
		if got := authedRepoURL(c.in); got != c.want {
			t.Errorf("authedRepoURL(token=%q, repoToken=%q, %q)=%q want %q", c.token, c.repoToken, c.in, got, c.want)
		}
	}
}

func TestParsePixiuctlVersion(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"pixiuctl version 0.2.4\n", "0.2.4", true},
		{"pixiuctl version 0.2.4", "0.2.4", true},
		{"  pixiuctl version 1.0.0  \n", "1.0.0", true},
		{"bad", "", false},
		{"pixiuctl version", "", false},
	}
	for _, c := range cases {
		got, err := parsePixiuctlVersion(c.in)
		if c.ok {
			if err != nil {
				t.Errorf("parse(%q) err=%v", c.in, err)
				continue
			}
			if got != c.want {
				t.Errorf("parse(%q)=%q want %q", c.in, got, c.want)
			}
		} else if err == nil {
			t.Errorf("parse(%q) 应失败，got %q", c.in, got)
		}
	}
}

func TestPixiuctlGitHubTag(t *testing.T) {
	old := githubTag
	t.Cleanup(func() { githubTag = old })

	githubTag = ""
	if got := pixiuctlGitHubTag("0.2.4"); got != "pixiuctl-0.2.4" {
		t.Fatalf("default = %q", got)
	}

	githubTag = "  custom-tag  "
	if got := pixiuctlGitHubTag("0.2.4"); got != "custom-tag" {
		t.Fatalf("override = %q", got)
	}
}

func TestPixiuctlBinaryAssetName(t *testing.T) {
	if got := pixiuctlBinaryAssetName("0.2.4", "linux", "amd64"); got != "pixiuctl-0.2.4-linux-amd64" {
		t.Fatalf("got %q", got)
	}
	if got := pixiuctlBinaryAssetName("0.2.4", "darwin", "arm64"); got != "pixiuctl-0.2.4-darwin-arm64" {
		t.Fatalf("got %q", got)
	}
}
