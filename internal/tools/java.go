package tools

import (
	"fmt"
	"path/filepath"

	"github.com/piotrkowalczuk/zordon/internal/zfs"
)

const mavenSettingsXML = `<?xml version="1.0" encoding="UTF-8"?>
<!-- zordon-owned user settings: keeps Maven off ~/.m2/settings.xml.
     Declare mirrors, servers and proxies through the Alphasfile instead. -->
<settings xmlns="http://maven.apache.org/SETTINGS/1.0.0"/>
`

// ensureMavenSettings writes the empty user settings.xml javaEnv points
// MAVEN_ARGS at, once. Maven aborts on a missing `-s` file, so the file
// has to exist before the first wrapper run; an existing file is left
// alone so a hand-edited one survives.
func ensureMavenSettings(path string) error {
	if zfs.Exists(path) {
		return nil
	}
	if err := zfs.EnsureDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("maven settings dir: %w", err)
	}
	if err := zfs.AtomicWrite(path, []byte(mavenSettingsXML)); err != nil {
		return fmt.Errorf("write maven settings: %w", err)
	}
	return nil
}
