package domain

// Container runtime names.
const (
	RuntimeDocker     = "docker"
	RuntimeContainerd = "containerd"
	RuntimeIncus      = "incus"
	RuntimeKubernetes = "kubernetes"
	RuntimeNone       = "none"
)

// KubernetesDefaultVersion is the default Kubernetes (k3s) version.
const KubernetesDefaultVersion = "v1.33.4+k3s1"
