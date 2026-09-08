package main

import "testing"

// TestFilterAssets exercises the substring-vs-token tradeoff: the new
// token-based filter must NOT match "64" inside "arm64", and it must
// still match Xray's standalone "64" arch token.
func TestFilterAssets(t *testing.T) {
	tests := []struct {
		name      string
		assets    []asset
		platforms string
		archs     string
		want      []string
	}{
		{
			name: "sing-box --archs=amd64 default must drop arm64 (regression guard)",
			assets: []asset{
				{Name: "sing-box-1.14.0-windows-amd64.zip"},
				{Name: "sing-box-1.14.0-windows-arm64.zip"},
				{Name: "sing-box-1.14.0-linux-amd64.tar.gz"},
				{Name: "sing-box-1.14.0-linux-arm64.tar.gz"},
			},
			platforms: "windows",
			archs:     "amd64",
			want: []string{
				"sing-box-1.14.0-windows-amd64.zip",
			},
		},
		{
			name: "Xray standalone 64 must match when --archs=amd64",
			assets: []asset{
				{Name: "Xray-linux-64.zip"},
				{Name: "Xray-linux-arm64-v8a.zip"},
				{Name: "Xray-windows-64.zip"},
				{Name: "Xray-windows-arm64-v8a.zip"},
				{Name: "Xray-win7-64.zip"},
			},
			platforms: "windows,linux",
			archs:     "amd64",
			want: []string{
				"Xray-linux-64.zip",
				"Xray-windows-64.zip",
				"Xray-win7-64.zip",
			},
		},
		{
			name: "--platforms=darwin must match Xray macos* files",
			assets: []asset{
				{Name: "Xray-macos-64.zip"},
				{Name: "Xray-macos-arm64-v8a.zip"},
				{Name: "Xray-linux-64.zip"},
				{Name: "gh_2.100.0_macOS_amd64.zip"},
			},
			platforms: "darwin",
			archs:     "amd64,arm64",
			want: []string{
				"Xray-macos-64.zip",
				"Xray-macos-arm64-v8a.zip",
				"gh_2.100.0_macOS_amd64.zip",
			},
		},
		{
			name: "cli/cli underscore-separated naming still works",
			assets: []asset{
				{Name: "gh_2.100.0_windows_amd64.zip"},
				{Name: "gh_2.100.0_windows_arm64.zip"},
				{Name: "gh_2.100.0_linux_amd64.tar.gz"},
				{Name: "gh_2.100.0_linux_arm64.tar.gz"},
			},
			platforms: "windows,linux",
			archs:     "amd64",
			// pickBestPerPlatform sorts by osOrder: windows (0) before linux (1).
			want: []string{
				"gh_2.100.0_windows_amd64.zip",
				"gh_2.100.0_linux_amd64.tar.gz",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterAssets(tt.assets, splitSet(tt.platforms), splitSet(tt.archs))
			if len(got) != len(tt.want) {
				t.Fatalf("got %d assets, want %d:\n got: %v\nwant: %v",
					len(got), len(tt.want), assetNames(got), tt.want)
			}
			for i, a := range got {
				if a.Name != tt.want[i] {
					t.Errorf("[%d] got %s, want %s", i, a.Name, tt.want[i])
				}
			}
		})
	}
}

func TestParseAssetPlatform(t *testing.T) {
	tests := []struct {
		name     string
		file     string
		wantOS   string
		wantArch string
	}{
		{"sing-box linux amd64", "sing-box-1.14.0-linux-amd64.tar.gz", "linux", "amd64"},
		{"sing-box windows arm64", "sing-box-1.14.0-windows-arm64.zip", "windows", "arm64"},
		{"Xray linux 64", "Xray-linux-64.zip", "linux", "amd64"},
		{"Xray linux arm32", "Xray-linux-arm32-v5.zip", "linux", "arm32"},
		{"Xray macos 64", "Xray-macos-64.zip", "darwin", "amd64"},
		{"Xray macos arm64", "Xray-macos-arm64-v8a.zip", "darwin", "arm64"},
		{"Xray win7 64", "Xray-win7-64.zip", "windows", "amd64"},
		{"Xray win7 32", "Xray-win7-32.zip", "windows", "386"},
		{"cli underscore windows amd64", "gh_2.100.0_windows_amd64.zip", "windows", "amd64"},
		{"cli macOS arm64", "gh_2.100.0_macOS_arm64.zip", "darwin", "arm64"},
		{"fzf windows amd64", "fzf-0.74.3-windows_amd64.zip", "windows", "amd64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotOS, gotArch, ok := parseAssetPlatform(tt.file)
			if !ok {
				t.Fatalf("parseAssetPlatform(%q) returned !ok", tt.file)
			}
			if gotOS != tt.wantOS {
				t.Errorf("os: got %q, want %q", gotOS, tt.wantOS)
			}
			if gotArch != tt.wantArch {
				t.Errorf("arch: got %q, want %q", gotArch, tt.wantArch)
			}
		})
	}
}

func TestPickBestPerPlatform(t *testing.T) {
	assets := []asset{
		{Name: "sing-box-1.14.0-linux-amd64-glibc.tar.gz", Size: 30900000},
		{Name: "sing-box-1.14.0-linux-amd64-musl.tar.gz", Size: 31000000},
		{Name: "sing-box-1.14.0-linux-amd64.tar.gz", Size: 30200000},
		{Name: "sing-box-1.14.0-linux-arm64.tar.gz", Size: 27700000},
		{Name: "sing-box-1.14.0-windows-amd64-legacy-windows-7.zip", Size: 26200000},
		{Name: "sing-box-1.14.0-windows-amd64.zip", Size: 31300000},
		{Name: "sing-box_1.14.0_linux_amd64.deb", Size: 30800000}, // package format, skip
	}
	got := pickBestPerPlatform(assets)
	// Order: windows (osOrder=0) before linux (osOrder=1).
	wantKeys := []string{"windows/amd64", "linux/amd64", "linux/arm64"}
	if len(got) != len(wantKeys) {
		t.Fatalf("got %d groups, want %d: %v", len(got), len(wantKeys), assetNames(got))
	}
	for i, a := range got {
		os, arch, _ := parseAssetPlatform(a.Name)
		key := os + "/" + arch
		if key != wantKeys[i] {
			t.Errorf("[%d] got key %q, want %q (file %s)", i, key, wantKeys[i], a.Name)
		}
	}
	// linux/amd64 should pick the generic one, not glibc/musl.
	for _, a := range got {
		os, arch, _ := parseAssetPlatform(a.Name)
		if os == "linux" && arch == "amd64" {
			if a.Name != "sing-box-1.14.0-linux-amd64.tar.gz" {
				t.Errorf("linux/amd64 should pick generic tar.gz, got %s", a.Name)
			}
		}
		// windows/amd64 should pick the non-legacy one.
		if os == "windows" && arch == "amd64" {
			if a.Name != "sing-box-1.14.0-windows-amd64.zip" {
				t.Errorf("windows/amd64 should pick non-legacy zip, got %s", a.Name)
			}
		}
	}
}

func assetNames(a []asset) []string {
	out := make([]string, len(a))
	for i, x := range a {
		out[i] = x.Name
	}
	return out
}
