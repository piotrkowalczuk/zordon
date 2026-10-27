package main

import (
	"fmt"
	"strings"
)

// pullFlag is `zordon start --pull`: what the start moves to the newest
// commit of its ref before it builds, and locks there. srv is the git source
// of every service it starts that follows a branch; pkg is every repository
// the invocation's Alphasfile requires. A bare --pull, like --pull=all, moves
// both.
type pullFlag struct {
	srv, pkg bool
}

func (p *pullFlag) String() string {
	switch {
	case p.srv && p.pkg:
		return "all"
	case p.srv:
		return "srv"
	case p.pkg:
		return "pkg"
	}
	return ""
}

func (p *pullFlag) Set(v string) error {
	switch v {
	case "true", "all":
		p.srv, p.pkg = true, true
		return nil
	case "false":
		p.srv, p.pkg = false, false
		return nil
	}
	for part := range strings.SplitSeq(v, ",") {
		switch strings.TrimSpace(part) {
		case "srv":
			p.srv = true
		case "pkg":
			p.pkg = true
		case "all":
			p.srv, p.pkg = true, true
		default:
			return fmt.Errorf("--pull takes srv, pkg or all, comma-separated; got %q", part)
		}
	}
	return nil
}

// IsBoolFlag lets --pull stand alone, meaning all.
func (p *pullFlag) IsBoolFlag() bool { return true }
