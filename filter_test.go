package main

import "testing"

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

// TestFilterByConfigured pins the user-configured name as an exact
// specification. The configured name is the only selection signal —
// no platform/arch pre-filter, no pick-best, no lex tiebreaker can
// override it. Exact (case-insensitive) match is the baseline;
// TestFilterByConfiguredGlob covers glob support.
func TestFilterByConfigured(t *testing.T) {
	assets := []asset{
		{Name: "dbflux-windows-amd64-setup.exe"},
		{Name: "dbflux-windows-amd64.zip"},
		{Name: "dbflux-windows-amd64-setup.exe.asc"},
		{Name: "dbflux-windows-amd64.zip.asc"},
	}
	got := filterByConfigured(assets, []string{"dbflux-windows-amd64.zip"})
	want := []string{"dbflux-windows-amd64.zip"}
	if len(got) != len(want) {
		t.Fatalf("got %d, want %d: %v", len(got), len(want), assetNames(got))
	}
	if got[0].Name != want[0] {
		t.Errorf("got %s, want %s", got[0].Name, want[0])
	}

	// Case-insensitive too — typos in capitalization must not silently
	// drop the match.
	got = filterByConfigured(assets, []string{"DBFLUX-WINDOWS-AMD64.ZIP"})
	if len(got) != 1 || got[0].Name != "dbflux-windows-amd64.zip" {
		t.Errorf("case-insensitive match failed: %v", assetNames(got))
	}

	// Empty configured list returns nothing (the caller short-circuits
	// before this in runBatch via len(configured)==0).
	if got := filterByConfigured(assets, nil); got != nil {
		t.Errorf("nil configured should return nil, got %v", assetNames(got))
	}
}

// TestFilterByConfiguredGlob pins the glob support: a pattern like
// "Microsoft.WSL_*_ARM64.msixbundle" must match every WSL release
// version (2.7.13, 2.9.10, 2.9.11, ...) without the user having to
// update the DB every time. Also covers multi-match (one pattern
// picking several assets), per-pattern failure isolation, and that
// exact patterns still work (backward compat).
func TestFilterByConfiguredGlob(t *testing.T) {
	assets := []asset{
		{Name: "Microsoft.WSL_2.7.13.0_x64_ARM64.msixbundle"},
		{Name: "Microsoft.WSL_2.9.10.0_x64_ARM64.msixbundle"},
		{Name: "Microsoft.WSL_2.9.11.0_x64_ARM64.msixbundle"},
		{Name: "wsl.2.9.11.0.x64.msi"},            // msi, not msixbundle
		{Name: "wsl.2.9.11.0.arm64.msi"},          // msi, not msixbundle
		{Name: "Microsoft.WSL_2.9.11.0_x64_ARM64.msixbundle.sha256"},
	}
	tests := []struct {
		name       string
		configured []string
		want       []string
	}{
		{
			name:       "WSL family pattern matches every release",
			configured: []string{"Microsoft.WSL_*_ARM64.msixbundle"},
			want: []string{
				"Microsoft.WSL_2.7.13.0_x64_ARM64.msixbundle",
				"Microsoft.WSL_2.9.10.0_x64_ARM64.msixbundle",
				"Microsoft.WSL_2.9.11.0_x64_ARM64.msixbundle",
			},
		},
		{
			name:       "pin a single version with glob suffix",
			configured: []string{"Microsoft.WSL_2.9.11.0_*.msixbundle"},
			want: []string{
				"Microsoft.WSL_2.9.11.0_x64_ARM64.msixbundle",
			},
		},
		{
			name:       "exact pattern (no wildcards) still works",
			configured: []string{"wsl.2.9.11.0.x64.msi"},
			want: []string{
				"wsl.2.9.11.0.x64.msi",
			},
		},
		{
			name:       "single ? matches one char (covers both arch prefixes)",
			configured: []string{"wsl.2.9.11.0.???64.msi"},
			want: []string{
				"wsl.2.9.11.0.arm64.msi",
			},
		},
		{
			name:       "* matches variable-width segment",
			configured: []string{"wsl.2.9.11.0.*64.msi"},
			want: []string{
				"wsl.2.9.11.0.x64.msi",
				"wsl.2.9.11.0.arm64.msi",
			},
		},
		{
			name:       "multi-pattern: bundle OR x64 msi",
			configured: []string{"Microsoft.WSL_*_ARM64.msixbundle", "wsl.2.9.11.0.x64.msi"},
			want: []string{
				"Microsoft.WSL_2.7.13.0_x64_ARM64.msixbundle",
				"Microsoft.WSL_2.9.10.0_x64_ARM64.msixbundle",
				"Microsoft.WSL_2.9.11.0_x64_ARM64.msixbundle",
				"wsl.2.9.11.0.x64.msi",
			},
		},
		{
			name:       "malformed pattern is skipped, others still match",
			configured: []string{"[unterminated", "Microsoft.WSL_*_ARM64.msixbundle"},
			want: []string{
				"Microsoft.WSL_2.7.13.0_x64_ARM64.msixbundle",
				"Microsoft.WSL_2.9.10.0_x64_ARM64.msixbundle",
				"Microsoft.WSL_2.9.11.0_x64_ARM64.msixbundle",
			},
		},
		{
			name:       "no match returns nil",
			configured: []string{"never-matches-*.zip"},
			want:       nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterByConfigured(assets, tt.configured)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d, want %d:\n got: %v\nwant: %v",
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

func assetNames(a []asset) []string {
	out := make([]string, len(a))
	for i, x := range a {
		out[i] = x.Name
	}
	return out
}
