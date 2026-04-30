package limautil

import "path/filepath"

const networkFile = "networks.yaml"

// NetworkFile returns path to the network file.
func NetworkFile(limaDir string) string {
	return filepath.Join(limaDir, "_config", networkFile)
}

// NetworkAssetsDirectory returns the directory for the generated network assets.
func NetworkAssetsDirectory(limaDir string) string {
	return filepath.Join(limaDir, "_networks")
}
