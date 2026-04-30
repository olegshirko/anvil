package root

import (
	"context"
	"log"
	"os"

	"anvil/internal/cli"
	"anvil/internal/environment/host"
	"anvil/internal/environment/vm/lima/limautil"
	"anvil/internal/infra"
	"anvil/internal/usecase"
	"anvil/internal/util/github"

	"github.com/sirupsen/logrus"
)

var (
	globalConfigHelper     *usecase.ConfigHelper
	globalConfigRepository usecase.ConfigRepository
	globalProfileManager   usecase.ProfileManager
	globalApplication      usecase.Application
)

// GetConfigRepository returns the global config repository.
func GetConfigRepository() usecase.ConfigRepository {
	return globalConfigRepository
}

// GetConfigHelper returns the global config helper.
func GetConfigHelper() *usecase.ConfigHelper {
	return globalConfigHelper
}

func init() {
	// Initialize the profile manager
	globalProfileManager = infra.NewProfileManager()
}

// NewApp creates a fully wired usecase.Application instance.
func NewApp() usecase.Application {
	// infra
	configDataStore := infra.NewConfigFileStore()
	configRepository := infra.NewConfigRepository(configDataStore, globalProfileManager)
	configHelper := configRepository.ConfigHelper()
	limautil.SetLimaHome(configRepository.GetLimaDir())
	configService := usecase.NewConfigService(configRepository, globalProfileManager)
	vmManager := infra.NewLimaVMManagerAdapter(configService, configRepository, configHelper)
	contManFactory := infra.NewContainerManagerFactory(vmManager, configService)

	var runtime string
	if conf, err := configService.LoadConfig(context.Background()); err == nil {
		runtime = conf.Runtime
	}
	containerManager, _ := contManFactory.Get(runtime)
	if containerManager == nil {
		containerManager, _ = contManFactory.Get("docker")
	}

	sshConfigManager := infra.NewSSHConfigManager(configRepository, globalProfileManager)
	pathValidator := infra.NewPathValidator(configRepository, globalProfileManager)
	statusProvider := infra.NewStatusProvider(configRepository, globalProfileManager, vmManager, containerManager)
	doctorService := infra.NewDoctorService(host.New(), configRepository, globalProfileManager, vmManager, containerManager)

	var imageDiscovery usecase.ImageDiscovery
	var imageRequester usecase.ImageRequester

	ghClient := github.NewClient(os.Getenv("GITHUB_TOKEN"))
	imageDiscovery = infra.NewGitHubImageDiscovery(ghClient)
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		imageRequester = infra.NewGitHubImageRequester(ghClient)
	} else {
		imageRequester = infra.NewBrowserImageRequester()
	}

	return usecase.NewApplication(
		configRepository,
		globalProfileManager,
		vmManager,
		containerManager,
		sshConfigManager,
		pathValidator,
		statusProvider,
		contManFactory,
		doctorService,
		imageDiscovery,
		imageRequester,
	)
}

// GetApp returns a fully wired usecase.Application instance.
func GetApp() usecase.Application {
	if globalApplication == nil {
		globalApplication = NewApp()
	}
	return globalApplication
}

// SetVerbose sets the verbose log level (called by Kong CLI).
func SetVerbose() {
	cli.Settings.Verbose = true
	logrus.SetLevel(logrus.DebugLevel)
	log.SetOutput(logrus.StandardLogger().Writer())
	log.SetFlags(0)
}
