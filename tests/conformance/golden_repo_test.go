package conformance_test

// goldenRepo at goldenRev is this repository on GitHub at a commit that
// carries golden/**. The git-source conformance tests clone it: a remote
// `git {}` primary must live on a supported host, so the local tree cannot
// stand in, and pinning a rev keeps the fixture immutable.
const (
	goldenRepo = "github.com/piotrkowalczuk/zordon"
	goldenRev  = "3fa7dbb8c2f3d539f6581087d94c9be899c88e1d"
)
