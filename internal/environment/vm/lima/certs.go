package lima

import (
	"context"
	"fmt"
	"path/filepath"

	"anvil/internal/util"
)

func (l limaVM) copyCerts() error {
	log := l.Logger(context.Background())

	home, err := util.UserHome()
	if err != nil {
		log.Warnln(fmt.Errorf("cannot detect home directory: %w", err))
		return nil
	}

	sourceDir := filepath.Join(home, ".docker", "certs.d")
	if _, err := l.host.Stat(sourceDir); err != nil {
		return nil // no certs to copy
	}

	cacheDir := filepath.Join(l.configService.CacheDir(), "docker-certs")
	_ = l.host.RunQuiet("rm", "-rf", cacheDir)
	if err := l.host.RunQuiet("mkdir", "-p", cacheDir); err != nil {
		log.Warnln(fmt.Errorf("cannot create certs cache: %w", err))
		return nil
	}
	if err := l.host.RunQuiet("cp", "-R", sourceDir+"/.", cacheDir); err != nil {
		log.Warnln(fmt.Errorf("cannot copy certs to cache: %w", err))
		return nil
	}

	for _, dest := range []string{"/etc/docker/certs.d", "/etc/ssl/certs"} {
		if err := l.RunQuiet("sudo", "mkdir", "-p", dest); err != nil {
			log.Warnln(fmt.Errorf("cannot create guest cert dir: %w", err))
			return nil
		}
		if err := l.RunQuiet("sudo", "cp", "-R", cacheDir+"/.", dest); err != nil {
			log.Warnln(fmt.Errorf("cannot copy certs to guest: %w", err))
			return nil
		}
	}

	return nil
}
