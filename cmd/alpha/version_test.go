package main

import "testing"

func TestVersionRequested(t *testing.T) {
	cases := map[string]struct {
		args []string
		want bool
	}{
		"long flag":                  {args: []string{"--version"}, want: true},
		"short flag":                 {args: []string{"-V"}, want: true},
		"bare word":                  {args: []string{"version"}, want: true},
		"no args":                    {args: nil, want: false},
		"the run subcommand":         {args: []string{"run"}, want: false},
		"only honored in first slot": {args: []string{"run", "--version"}, want: false},
	}

	for hint, c := range cases {
		t.Run(hint, func(t *testing.T) {
			if got := versionRequested(c.args); got != c.want {
				t.Errorf("versionRequested(%q)=%v, want %v", c.args, got, c.want)
			}
		})
	}
}
