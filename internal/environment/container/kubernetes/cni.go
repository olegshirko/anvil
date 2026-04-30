package kubernetes

import (
	"fmt"
	"path/filepath"

	"anvil/internal/cli"
	"anvil/internal/embedded"
	"anvil/internal/environment"
)

// setupCNI writes the embedded Flannel CNI configuration to the guest.
func setupCNI(guest environment.GuestActions, pipe *cli.ActiveCommandChain) {
	pipe.Add(func() error {
		cniPath := "/etc/cni/net.d/10-flannel.conflist"
		dir := filepath.Dir(cniPath)

		data, err := embedded.Read("k3s/flannel.json")
		if err != nil {
			return fmt.Errorf("cannot read embedded flannel config: %w", err)
		}

		if err := guest.Run("sudo", "mkdir", "-p", dir); err != nil {
			return fmt.Errorf("cannot create CNI directory: %w", err)
		}

		return guest.Write(cniPath, data)
	})
}
