package specutil

import (
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// UTSNamespaceShared reports a UTS namespace joined by path (host uts or
// container:<x>): the hostname is shared, so it must not be set.
func UTSNamespaceShared(spec *specs.Spec) bool {
	if spec.Linux == nil {
		return false
	}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.UTSNamespace && ns.Path != "" {
			return true
		}
	}
	return false
}
