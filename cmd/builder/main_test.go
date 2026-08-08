package main

import (
	"strings"
	"testing"

	"builder/internal/config"
)

// TestResolveBuildOptionsPriority 验证 build 参数合并优先级：命令行 > 配置 > 默认值。
func TestResolveBuildOptionsPriority(t *testing.T) {
	cfg := &config.Config{
		Build: config.BuildOptions{
			OS:                "ubuntu",
			OSVersion:         "22.04",
			KubernetesVersion: "v1.27.3",
			Arch:              "arm64",
			Mirror:            "official",
			WorkDir:           "./work-cfg",
			OutDir:            "./dist-cfg",
			Mode:              "packages",
			SkipAddons:        true,
			DryRun:            true,
		},
	}

	// 与 flag 定义一致的内置默认值（非显式设置时 vals 即为这些值）。
	defVals := buildFlagValues{
		OS: "", OSVersion: "", K8sVersion: "",
		Arch: "amd64", Mirror: "official",
		WorkDir: "./work", OutDir: "./dist", Mode: "all",
	}

	cases := []struct {
		name    string
		vals    buildFlagValues
		changed buildFlagChanged
		want    buildOptions
	}{
		{
			name:    "配置覆盖默认（命令行未传）",
			vals:    defVals,
			changed: buildFlagChanged{},
			want: buildOptions{
				OS: "ubuntu", OSVersion: "22.04", K8sVersion: "v1.27.3",
				Arch: "arm64", Mirror: "official",
				WorkDir: "./work-cfg", OutDir: "./dist-cfg", Mode: "packages",
				SkipAddons: true, DryRun: true,
			},
		},
		{
			name: "命令行优先于配置",
			vals: buildFlagValues{
				OS: "rocky", OSVersion: "9", K8sVersion: "v1.31.0",
				Arch: "amd64", Mirror: "official",
				WorkDir: "./work", OutDir: "./dist", Mode: "all",
			},
			changed: buildFlagChanged{OS: true, OSVersion: true, K8sVersion: true, Arch: true, Mode: true},
			want: buildOptions{
				OS: "rocky", OSVersion: "9", K8sVersion: "v1.31.0",
				Arch: "amd64", Mirror: "official",
				WorkDir: "./work-cfg", OutDir: "./dist-cfg", Mode: "all",
				SkipAddons: true, DryRun: true,
			},
		},
		{
			name: "命令行 bool 显式 false 覆盖配置 true",
			vals: buildFlagValues{
				OS: "", OSVersion: "", K8sVersion: "",
				Arch: "amd64", Mirror: "official",
				WorkDir: "./work", OutDir: "./dist", Mode: "all",
				SkipAddons: false,
			},
			changed: buildFlagChanged{SkipAddons: true},
			want: buildOptions{
				OS: "ubuntu", OSVersion: "22.04", K8sVersion: "v1.27.3",
				Arch: "arm64", Mirror: "official",
				WorkDir: "./work-cfg", OutDir: "./dist-cfg", Mode: "packages",
				SkipAddons: false, DryRun: true,
			},
		},
		{
			name: "k8s 版本命令行显式即生效（--kubernetes-version）",
			vals: buildFlagValues{
				OS: "", OSVersion: "", K8sVersion: "v1.30.0",
				Arch: "amd64", Mirror: "official",
				WorkDir: "./work", OutDir: "./dist", Mode: "all",
			},
			changed: buildFlagChanged{K8sVersion: true},
			want: buildOptions{
				OS: "ubuntu", OSVersion: "22.04", K8sVersion: "v1.30.0",
				Arch: "arm64", Mirror: "official",
				WorkDir: "./work-cfg", OutDir: "./dist-cfg", Mode: "packages",
				SkipAddons: true, DryRun: true,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveBuildOptions(cfg, c.vals, c.changed)
			if got != c.want {
				t.Errorf("resolveBuildOptions 结果异常:\n got %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// TestResolveBuildOptionsEmptyConfig 配置为空（或未配置）时回退 flag 默认值。
func TestResolveBuildOptionsEmptyConfig(t *testing.T) {
	cfg := &config.Config{} // 未配置 build 节

	vals := buildFlagValues{
		OS: "", OSVersion: "", K8sVersion: "",
		Arch: "amd64", Mirror: "official",
		WorkDir: "./work", OutDir: "./dist", Mode: "all",
	}

	got := resolveBuildOptions(cfg, vals, buildFlagChanged{})
	want := buildOptions{
		OS: "", OSVersion: "", K8sVersion: "",
		Arch: "amd64", Mirror: "official",
		WorkDir: "./work", OutDir: "./dist", Mode: "all",
		SkipAddons: false, DryRun: false,
	}
	if got != want {
		t.Errorf("空配置合并结果异常:\n got %+v\nwant %+v", got, want)
	}

	// 配置 os 为空字符串时，命令行为空则最终 os 仍为空（供必填校验报错）。
	cfg.Build.OS = ""
	if got := resolveBuildOptions(cfg, vals, buildFlagChanged{}); got.OS != "" {
		t.Errorf("配置 os 为空时应保持空，实际 %q", got.OS)
	}
}

// TestRequiredParamsMissing 验证必填参数（os/os-version/kubernetes-version）缺失时的报错信息。
func TestRequiredParamsMissing(t *testing.T) {
	buildK8sVersion = ""
	buildOS = ""
	buildOSVersion = ""
	buildArch = "amd64"

	// 空配置 + 未传任何必填 flag → packages 三个必填项全部列出。
	cfg := &config.Config{}
	opts := resolveBuildOptions(cfg, buildFlagValues{
		K8sVersion: buildK8sVersion, OS: buildOS, OSVersion: buildOSVersion,
		Arch: "amd64", Mirror: "official", WorkDir: "./work", OutDir: "./dist", Mode: "packages",
	}, buildFlagChanged{})
	if opts.OS != "" || opts.OSVersion != "" || opts.K8sVersion != "" {
		t.Fatalf("期望全部必填为空，实际 %+v", opts)
	}
	missing := requiredMissing(opts, "packages")
	if strings.Join(missing, ",") != "kubernetes-version,os,os-version" {
		t.Errorf("缺失项列出顺序异常: %v", missing)
	}

	// images 模式：仅 kubernetes-version 必填。
	missing = requiredMissing(opts, "images")
	if strings.Join(missing, ",") != "kubernetes-version" {
		t.Errorf("images 模式缺失项异常: %v", missing)
	}
}

// TestRequiredParamsMissingOnlyAddons 验证 --only-addons 时 kubernetes-version 不再必填：
// only-addons 且缺 k8s 版本校验通过；only-addons 缺 os 仅报 os；非 only-addons 缺 k8s 仍报错。
func TestRequiredParamsMissingOnlyAddons(t *testing.T) {
	base := buildOptions{OS: "ubuntu", OSVersion: "24.04", Mode: "packages"}

	// only-addons + 缺 k8s 版本：校验通过（kubernetes-version 不列入缺失）。
	opts := base
	opts.OnlyAddons = true
	if missing := requiredMissing(opts, "packages"); len(missing) != 0 {
		t.Errorf("only-addons 缺 k8s 版本应校验通过，实际缺失 %v", missing)
	}

	// only-addons + 缺 os：仅列出 os（kubernetes-version 不再出现）。
	opts = base
	opts.OnlyAddons = true
	opts.OS = ""
	if missing := requiredMissing(opts, "packages"); strings.Join(missing, ",") != "os" {
		t.Errorf("only-addons 缺 os 应仅报 os，实际 %v", missing)
	}

	// 非 only-addons + 缺 k8s 版本：仍报 kubernetes-version。
	opts = base
	opts.OnlyAddons = false
	if missing := requiredMissing(opts, "packages"); strings.Join(missing, ",") != "kubernetes-version" {
		t.Errorf("非 only-addons 缺 k8s 版本应报 kubernetes-version，实际 %v", missing)
	}
}
