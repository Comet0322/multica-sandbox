package multica

import "strings"

// NoLongerUsed is the one warning for removed settings that are still set,
// in the environment or in the overrides file: they are named and ignored,
// so an old deployment still starts.
func NoLongerUsed(names ...string) string {
	if len(names) == 1 {
		return names[0] + " is no longer used and is ignored"
	}
	return strings.Join(names, ", ") + " are no longer used and are ignored"
}
