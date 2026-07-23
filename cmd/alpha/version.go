package main

// versionRequested reports whether argv is asking for the build identity.
//
// Checked before ff parses, for the same reason as in zordon: alpha also
// runs with ff.WithEnvVarPrefix("ZORDON"), so a --version root flag would
// answer to $ZORDON_VERSION and an exported value would break every alpha
// invocation.
func versionRequested(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "-V", "--version", "version":
		return true
	}
	return false
}
