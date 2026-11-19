package queue

import (
	"fmt"
	"strconv"
)

// formatVersion is the on-disk layout this build reads and writes. Version 1
// is the first stamped layout and matches what v0.1.0 wrote.
const formatVersion = 1

// checkFormat reports whether a stored version can be read. missing is true
// for a store that predates the stamp; it holds the version 1 layout, so the
// caller stamps it and reads it unchanged.
func checkFormat(stored []byte) (missing bool, err error) {
	if len(stored) == 0 {
		return true, nil
	}
	version, err := strconv.Atoi(string(stored))
	if err != nil {
		return false, fmt.Errorf("queue format version %q is not a number", stored)
	}
	if version > formatVersion {
		return false, fmt.Errorf("queue format version %d is newer than this build reads (%d); upgrade mayflyd", version, formatVersion)
	}
	return false, migrateFormat(version)
}

// migrateFormat moves a store written by an older build to formatVersion. Each
// case rewrites one version's difference. Version 1 is the first stamped
// layout, so there is nothing to rewrite yet; a later version adds its case
// before raising formatVersion.
func migrateFormat(from int) error {
	switch from {
	case 1:
		return nil
	}
	return fmt.Errorf("queue format version %d has no migration path", from)
}
