package store

import "strings"

// ValidRoleKey reports whether key is a well-formed role key.
func ValidRoleKey(key string) bool { return roleKeyPattern.MatchString(key) }

// ValidRoleFields reports whether the free-form fields of a role definition are within their limits.
func ValidRoleFields(label, emoji, body, cardinality string) bool {
	return strings.TrimSpace(label) != "" && len(label) <= 80 && len(body) <= 32768 && len(emoji) <= 32 &&
		(cardinality == "multi" || cardinality == "singleton")
}
