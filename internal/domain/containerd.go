package domain

// HostSocketFiles represents the paths to the host socket files for containerd.
type HostSocketFiles struct {
	Containerd string
	Buildkitd  string
}
