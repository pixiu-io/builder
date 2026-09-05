package main

import (
	"testing"
)

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
