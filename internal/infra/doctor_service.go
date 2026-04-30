package infra

import (
	"context"
	"fmt"
	"runtime"

	"anvil/internal/core"
	"anvil/internal/domain"
	"anvil/internal/environment"
	"anvil/internal/usecase"
	"anvil/internal/util"
)

// ensure DoctorServiceImpl implements usecase.DoctorService
var _ usecase.DoctorService = (*DoctorServiceImpl)(nil)

// DoctorServiceImpl is an infrastructure implementation of usecase.DoctorService.
type DoctorServiceImpl struct {
	host       environment.HostActions
	configRepo usecase.ConfigRepository
	profileMan usecase.ProfileManager
	vmMan      usecase.VMManager
	contMan    usecase.ContainerManager
}

// NewDoctorService creates a new DoctorServiceImpl.
func NewDoctorService(
	host environment.HostActions,
	configRepo usecase.ConfigRepository,
	profileMan usecase.ProfileManager,
	vmMan usecase.VMManager,
	contMan usecase.ContainerManager,
) *DoctorServiceImpl {
	return &DoctorServiceImpl{
		host:       host,
		configRepo: configRepo,
		profileMan: profileMan,
		vmMan:      vmMan,
		contMan:    contMan,
	}
}

// Diagnose checks host readiness and returns a list of diagnoses.
func (d *DoctorServiceImpl) Diagnose(ctx context.Context) ([]domain.Diagnosis, error) {
	var diagnoses []domain.Diagnosis

	// Lima version check
	if err := core.ValidateLimaVersion(); err != nil {
		diagnoses = append(diagnoses, domain.Diagnosis{
			Check:   "lima version",
			Status:  "fail",
			Message: err.Error(),
			Fix:     "brew install lima || brew upgrade lima",
		})
	} else {
		diagnoses = append(diagnoses, domain.Diagnosis{
			Check:  "lima version",
			Status: "ok",
		})
	}

	// QEMU/VZ check
	if runtime.GOOS == "darwin" {
		if util.AtLeastVentura() {
			diagnoses = append(diagnoses, domain.Diagnosis{
				Check:   "virtualization",
				Status:  "ok",
				Message: "Apple Virtualization (vz) is available",
			})
		} else {
			diagnoses = append(diagnoses, domain.Diagnosis{
				Check:   "virtualization",
				Status:  "warn",
				Message: "macOS 13+ recommended for vz; falling back to qemu",
			})
		}
	}

	// Docker client check
	currentRuntime, _ := d.contMan.CurrentRuntime(ctx)
	if currentRuntime == "docker" {
		if _, err := d.host.RunOutput("docker", "--version"); err != nil {
			diagnoses = append(diagnoses, domain.Diagnosis{
				Check:   "docker client",
				Status:  "warn",
				Message: "docker CLI not found on host",
				Fix:     "brew install docker",
			})
		} else {
			diagnoses = append(diagnoses, domain.Diagnosis{
				Check:  "docker client",
				Status: "ok",
			})
		}
	}

	// kubectl check (if kubernetes enabled)
	profile := d.profileMan.GetCurrentProfile(ctx)
	conf, _ := d.configRepo.LoadInstanceState(ctx, profile)
	if conf.Kubernetes.Enabled {
		if _, err := d.host.RunOutput("kubectl", "version", "--client"); err != nil {
			diagnoses = append(diagnoses, domain.Diagnosis{
				Check:   "kubectl",
				Status:  "warn",
				Message: "kubectl not found on host",
				Fix:     "brew install kubectl",
			})
		} else {
			diagnoses = append(diagnoses, domain.Diagnosis{
				Check:  "kubectl",
				Status: "ok",
			})
		}
	}

	// Socket directory check
	socketDir := d.configRepo.ProfileConfigDir(profile)
	if socketDir != "" {
		if info, err := d.host.Stat(socketDir); err != nil || !info.IsDir() {
			diagnoses = append(diagnoses, domain.Diagnosis{
				Check:   "socket directory",
				Status:  "warn",
				Message: fmt.Sprintf("socket directory %s not accessible", socketDir),
			})
		} else {
			diagnoses = append(diagnoses, domain.Diagnosis{
				Check:  "socket directory",
				Status: "ok",
			})
		}
	}

	return diagnoses, nil
}
