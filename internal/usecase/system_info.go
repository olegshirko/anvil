package usecase

// HomeDirProvider provides the user's home directory.
type HomeDirProvider interface {
	GetHomeDir() (string, error)
}

// OSInfoProvider provides operating system and hardware information.
type OSInfoProvider interface {
	IsMacOS13OrNewer() bool
	IsMacOS13OrNewerOnArm() bool
}
