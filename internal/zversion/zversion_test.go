package zversion

import (
	"runtime"
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	cases := map[string]struct {
		ldVersion, ldCommit, ldDate string
		info                        *debug.BuildInfo
		wantVersion                 string
		wantCommit                  string
		wantDate                    string
	}{
		"ldflags win over build info": {
			ldVersion:   "v0.18.0",
			ldCommit:    "9b4437bd1c2e",
			ldDate:      "2026-07-23T20:00:00Z",
			info:        buildInfo("v0.17.1", "aaaaaaaaaaaaaaaaaaaa", "2020-01-01T00:00:00Z", "false"),
			wantVersion: "v0.18.0",
			wantCommit:  "9b4437bd1c2e",
			wantDate:    "2026-07-23T20:00:00Z",
		},
		"go install stamps the module version": {
			info:        buildInfo("v0.17.1", "", "", ""),
			wantVersion: "v0.17.1",
		},
		"local build falls back to vcs settings": {
			info:        buildInfo("(devel)", "7c826464877ee707b5d7234ce0c2616109a1ac1d", "2026-07-21T21:03:55Z", "false"),
			wantVersion: "(devel)",
			wantCommit:  "7c826464877e",
			wantDate:    "2026-07-21T21:03:55Z",
		},
		"dirty tree marks the commit": {
			info:        buildInfo("(devel)", "7c826464877ee707b5d7234ce0c2616109a1ac1d", "", "true"),
			wantVersion: "(devel)",
			wantCommit:  "7c826464877e+dirty",
		},
		"short commit is not truncated": {
			info:        buildInfo("(devel)", "abc123", "", "false"),
			wantVersion: "(devel)",
			wantCommit:  "abc123",
		},
		"dirty without a revision stays unmarked": {
			info:        buildInfo("(devel)", "", "", "true"),
			wantVersion: "(devel)",
		},
		"no build info at all": {
			wantVersion: "(devel)",
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			got := resolve(c.ldVersion, c.ldCommit, c.ldDate, reader(c.info))

			if got.Version != c.wantVersion {
				t.Errorf("Version=%q, want %q", got.Version, c.wantVersion)
			}
			if got.Commit != c.wantCommit {
				t.Errorf("Commit=%q, want %q", got.Commit, c.wantCommit)
			}
			if got.Date != c.wantDate {
				t.Errorf("Date=%q, want %q", got.Date, c.wantDate)
			}
			if got.Go != runtime.Version() {
				t.Errorf("Go=%q, want %q", got.Go, runtime.Version())
			}
		})
	}
}

func TestInfo_String(t *testing.T) {
	cases := map[string]struct {
		info Info
		want string
	}{
		"all fields": {
			info: Info{Version: "v0.18.0", Commit: "9b4437bd1c2e", Date: "2026-07-23T20:00:00Z", Go: "go1.26.5"},
			want: "v0.18.0 (9b4437bd1c2e, 2026-07-23T20:00:00Z, go1.26.5)",
		},
		"version only": {
			info: Info{Version: "v0.18.0"},
			want: "v0.18.0",
		},
		"missing date is skipped": {
			info: Info{Version: "(devel)", Commit: "abc123", Go: "go1.26.5"},
			want: "(devel) (abc123, go1.26.5)",
		},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := c.info.String(); got != c.want {
				t.Errorf("String()=%q, want %q", got, c.want)
			}
		})
	}
}

func TestLine(t *testing.T) {
	got := Line("zordon")
	if want := "zordon " + Get().String(); got != want {
		t.Errorf("Line()=%q, want %q", got, want)
	}
}

func buildInfo(mainVersion, revision, vcsTime, modified string) *debug.BuildInfo {
	info := &debug.BuildInfo{Main: debug.Module{Version: mainVersion}}
	for key, value := range map[string]string{
		"vcs.revision": revision,
		"vcs.time":     vcsTime,
		"vcs.modified": modified,
	} {
		if value != "" {
			info.Settings = append(info.Settings, debug.BuildSetting{Key: key, Value: value})
		}
	}
	return info
}

func reader(info *debug.BuildInfo) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) { return info, info != nil }
}
