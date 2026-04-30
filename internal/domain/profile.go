package domain

// AppName is the name of the application.
const AppName = "anvil"

// Profile is anvil profile.
type Profile struct {
	ID          string
	DisplayName string
	ShortName   string

	Changed bool // indicates if the profile has been changed
}

// ProfileInfo is the information about a profile.
type ProfileInfo interface {
	// ConfigDir returns the configuration directory.
	ConfigDir() string

	// LimaInstanceDir returns the directory for the Lima instance.
	LimaInstanceDir() string

	// File returns the path to the config file.
	File() string

	// LimaFile returns the path to the lima config file.
	LimaFile() string

	// StateFile returns the path to the state file.
	StateFile() string

	// StoreFile returns the path to the store file.
	StoreFile() string
}
