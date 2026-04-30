package environment

type HostActions interface {
	runActions
	fileActions
	Env(string) (string, error)
	WithEnv(env ...string) HostActions
	WithDir(dir string) HostActions
}

// Host is the host environment.
type Host interface {
	HostActions
}
