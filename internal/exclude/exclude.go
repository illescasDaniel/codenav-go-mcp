// Package exclude lists directory names no tool should walk into.
package exclude

import (
	"path/filepath"
	"strings"
)

var dirNames = map[string]bool{
	"node_modules": true, ".git": true, "vendor": true, "dist": true, "build": true,
	".venv": true, "venv": true, "__pycache__": true,
}

// IsDirName reports whether a directory name is skipped.
func IsDirName(name string) bool { return dirNames[name] }

// IsExcluded reports whether path (absolute, or relative to root) has an excluded directory below root.
func IsExcluded(path, root string) bool {
	rel := path
	if r, err := filepath.Rel(root, path); err == nil {
		rel = r
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if dirNames[part] {
			return true
		}
	}
	return false
}
