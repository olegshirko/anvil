package usecase

// appVersion and revision are set at build time via ldflags.
var (
	appVersion = "development"
	revision   = "unknown"
)

// AppVersionInfo holds build-time version info.
type AppVersionInfo struct {
	Version  string
	Revision string
}

// GetAppVersion returns the current application version info.
func GetAppVersion() AppVersionInfo {
	return AppVersionInfo{Version: appVersion, Revision: revision}
}
