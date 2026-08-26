package config

import "go.yaml.in/yaml/v4"

// Migration messages for schema keys that were renamed or removed. The Fleet
// schema uses the glossary term Role (see CONTEXT.md); the pre-rename keys are
// kept on the config structs purely so Validate can name the replacement
// instead of falling back to WithKnownFields()'s generic unknown-field error.
const (
	migratePiType     = "host.yml: 'pi_type:' was renamed to 'role:'"
	migratePiTypes    = "assignments.yml: 'pi_types:' was renamed to 'roles:'"
	migratePrometheus = "fleet.yml: 'prometheus:' was removed from the schema; delete it"
)

// keyPresent reports whether a YAML key appeared in the document at all,
// independently of the value it carried. An absent key leaves the zero Node;
// a present key never does, so `pi_type:`, `pi_type: ""` and `pi_type: null`
// are all rejected alongside `pi_type: node`.
//
// Retired keys are declared as yaml.Node rather than their old value type so
// that presence is all Validate can observe: there is no decoded value to read,
// and therefore no way for a retired key to act as an alias for its
// replacement.
func keyPresent(n yaml.Node) bool {
	return !n.IsZero()
}
