package main

import (
	"runtime/debug"
	"testing"
)

func TestBuildVersion(t *testing.T) {
	const rev = "0123456789abcdef0123456789abcdef01234567"
	vcs := func(kv ...string) *debug.BuildInfo {
		info := &debug.BuildInfo{}
		for i := 0; i+1 < len(kv); i += 2 {
			info.Settings = append(info.Settings, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return info
	}
	for _, tc := range []struct {
		name string
		v    string
		info *debug.BuildInfo
		want string
	}{
		{"link-time version kept", "0.1.0", vcs("vcs.revision", rev), "0.1.0"},
		{"no build info", "dev", nil, "dev"},
		{"no revision recorded", "dev", vcs("-trimpath", "true"), "dev"},
		{"clean checkout", "dev", vcs("vcs", "git", "vcs.revision", rev, "vcs.modified", "false"), "dev+0123456789ab"},
		{"modified checkout", "dev", vcs("vcs.revision", rev, "vcs.modified", "true"), "dev+0123456789ab.dirty"},
		{"short revision", "dev", vcs("vcs.revision", "abc123"), "dev+abc123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildVersion(tc.v, tc.info); got != tc.want {
				t.Errorf("buildVersion(%q) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}
