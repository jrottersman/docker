// Package daemon exposes the functions that occur on the host server
// that the Docker daemon is running.
//
// In implementing the various functions of the daemon, there is often
// a method-specific struct for configuring the runtime behavior.
package daemon

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/distribution/digest"
	"github.com/docker/docker/api"
	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	eventtypes "github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	registrytypes "github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/container"
	"github.com/docker/docker/daemon/events"
	"github.com/docker/docker/daemon/exec"
	"github.com/docker/docker/daemon/execdriver"
	"github.com/docker/docker/daemon/execdriver/execdrivers"
	// register graph drivers
	_ "github.com/docker/docker/daemon/graphdriver/register"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/network"
	"github.com/docker/docker/distribution"
	dmetadata "github.com/docker/docker/distribution/metadata"
	"github.com/docker/docker/distribution/xfer"
	derr "github.com/docker/docker/errors"
	"github.com/docker/docker/image"
	"github.com/docker/docker/image/tarexport"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/migrate/v1"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/discovery"
	"github.com/docker/docker/pkg/fileutils"
	"github.com/docker/docker/pkg/graphdb"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/mount"
	"github.com/docker/docker/pkg/namesgenerator"
	"github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/pkg/signal"
	"github.com/docker/docker/pkg/streamformatter"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/pkg/sysinfo"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/docker/pkg/truncindex"
	"github.com/docker/docker/reference"
	"github.com/docker/docker/registry"
	"github.com/docker/docker/runconfig"
	"github.com/docker/docker/utils"
	volumedrivers "github.com/docker/docker/volume/drivers"
	"github.com/docker/docker/volume/local"
	"github.com/docker/docker/volume/store"
	"github.com/docker/go-connections/nat"
	"github.com/docker/libnetwork"
	lntypes "github.com/docker/libnetwork/types"
	"github.com/docker/libtrust"
	"github.com/opencontainers/runc/libcontainer"
	"golang.org/x/net/context"
)

const (
	// maxDownloadConcurrency is the maximum number of downloads that
	// may take place at a time for each pull.
	maxDownloadConcurrency = 3
	// maxUploadConcurrency is the maximum number of uploads that
	// may take place at a time for each push.
	maxUploadConcurrency = 5
)

var (
	validContainerNameChars   = utils.RestrictedNameChars
	validContainerNamePattern = utils.RestrictedNamePattern

	errSystemNotSupported = errors.New("The Docker daemon is not supported on this platform.")
)

// ErrImageDoesNotExist is error returned when no image can be found for a reference.
type ErrImageDoesNotExist struct {
	RefOrID string
}

func (e ErrImageDoesNotExist) Error() string {
	return fmt.Sprintf("no such id: %s", e.RefOrID)
}

type contStore struct {
	s map[string]*container.Container
	sync.Mutex
}

func (c *contStore) Add(id string, cont *container.Container) {
	c.Lock()
	c.s[id] = cont
	c.Unlock()
}

func (c *contStore) Get(id string) *container.Container {
	c.Lock()
	res := c.s[id]
	c.Unlock()
	return res
}

func (c *contStore) Delete(id string) {
	c.Lock()
	delete(c.s, id)
	c.Unlock()
}

func (c *contStore) List() []*container.Container {
	containers := new(History)
	c.Lock()
	for _, cont := range c.s {
		containers.Add(cont)
	}
	c.Unlock()
	containers.sort()
	return *containers
}

// Daemon holds information about the Docker daemon.
type Daemon struct {
	ID                        string
	repository                string
	containers                *contStore
	execCommands              *exec.Store
	referenceStore            reference.Store
	downloadManager           *xfer.LayerDownloadManager
	uploadManager             *xfer.LayerUploadManager
	distributionMetadataStore dmetadata.Store
	trustKey                  libtrust.PrivateKey
	idIndex                   *truncindex.TruncIndex
	configStore               *Config
	containerGraphDB          *graphdb.Database
	execDriver                execdriver.Driver
	statsCollector            *statsCollector
	defaultLogConfig          containertypes.LogConfig
	RegistryService           *registry.Service
	EventsService             *events.Events
	netController             libnetwork.NetworkController
	volumes                   *store.VolumeStore
	discoveryWatcher          discovery.Watcher
	root                      string
	shutdown                  bool
	uidMaps                   []idtools.IDMap
	gidMaps                   []idtools.IDMap
	layerStore                layer.Store
	imageStore                image.Store
}

// GetContainer looks for a container using the provided information, which could be
// one of the following inputs from the caller:
//  - A full container ID, which will exact match a container in daemon's list
//  - A container name, which will only exact match via the GetByName() function
//  - A partial container ID prefix (e.g. short ID) of any length that is
//    unique enough to only return a single container object
//  If none of these searches succeed, an error is returned
func (daemon *Daemon) GetContainer(prefixOrName string) (*container.Container, error) {
	if containerByID := daemon.containers.Get(prefixOrName); containerByID != nil {
		// prefix is an exact match to a full container ID
		return containerByID, nil
	}

	// GetByName will match only an exact name provided; we ignore errors
	if containerByName, _ := daemon.GetByName(prefixOrName); containerByName != nil {
		// prefix is an exact match to a full container Name
		return containerByName, nil
	}

	containerID, indexError := daemon.idIndex.Get(prefixOrName)
	if indexError != nil {
		// When truncindex defines an error type, use that instead
		if indexError == truncindex.ErrNotExist {
			return nil, derr.ErrorCodeNoSuchContainer.WithArgs(prefixOrName)
		}
		return nil, indexError
	}
	return daemon.containers.Get(containerID), nil
}

// Exists returns a true if a container of the specified ID or name exists,
// false otherwise.
func (daemon *Daemon) Exists(id string) bool {
	c, _ := daemon.GetContainer(id)
	return c != nil
}

// IsPaused returns a bool indicating if the specified container is paused.
func (daemon *Daemon) IsPaused(id string) bool {
	c, _ := daemon.GetContainer(id)
	return c.State.IsPaused()
}

func (daemon *Daemon) containerRoot(id string) string {
	return filepath.Join(daemon.repository, id)
}

// Load reads the contents of a container from disk
// This is typically done at startup.
func (daemon *Daemon) load(id string) (*container.Container, error) {
	container := daemon.newBaseContainer(id)

	if err := container.FromDisk(); err != nil {
		return nil, err
	}

	if container.ID != id {
		return container, fmt.Errorf("Container %s is stored at %s", container.ID, id)
	}

	return container, nil
}

func (daemon *Daemon) registerName(container *container.Container) error {
	if daemon.Exists(container.ID) {
		return fmt.Errorf("Container is already loaded")
	}
	if err := validateID(container.ID); err != nil {
		return err
	}
	if container.Name == "" {
		name, err := daemon.generateNewName(container.ID)
		if err != nil {
			return err
		}
		container.Name = name

		if err := container.ToDiskLocking(); err != nil {
			logrus.Errorf("Error saving container name to disk: %v", err)
		}
	}

	return nil
}

// Register makes a container object usable by the daemon as <container.ID>
func (daemon *Daemon) Register(container *container.Container) error {
	// Attach to stdout and stderr
	if container.Config.OpenStdin {
		container.NewInputPipes()
	} else {
		container.NewNopInputPipe()
	}
	daemon.containers.Add(container.ID, container)

	// don't update the Suffixarray if we're starting up
	// we'll waste time if we update it for every container
	daemon.idIndex.Add(container.ID)

	if container.IsRunning() {
		logrus.Debugf("killing old running container %s", container.ID)
		// Set exit code to 128 + SIGKILL (9) to properly represent unsuccessful exit
		container.SetStoppedLocking(&execdriver.ExitStatus{ExitCode: 137})
		// use the current driver and ensure that the container is dead x.x
		cmd := &execdriver.Command{
			CommonCommand: execdriver.CommonCommand{
				ID: container.ID,
			},
		}
		daemon.execDriver.Terminate(cmd)

		container.UnmountIpcMounts(mount.Unmount)

		daemon.Unmount(container)
		if err := container.ToDiskLocking(); err != nil {
			logrus.Errorf("Error saving stopped state to disk: %v", err)
		}
	}

	if err := daemon.prepareMountPoints(container); err != nil {
		return err
	}

	return nil
}

func (daemon *Daemon) restore() error {
	type cr struct {
		container  *container.Container
		registered bool
	}

	var (
		debug         = os.Getenv("DEBUG") != ""
		currentDriver = daemon.GraphDriverName()
		containers    = make(map[string]*cr)
	)

	if !debug {
		logrus.Info("Loading containers: start.")
	}
	dir, err := ioutil.ReadDir(daemon.repository)
	if err != nil {
		return err
	}

	for _, v := range dir {
		id := v.Name()
		container, err := daemon.load(id)
		if !debug && logrus.GetLevel() == logrus.InfoLevel {
			fmt.Print(".")
		}
		if err != nil {
			logrus.Errorf("Failed to load container %v: %v", id, err)
			continue
		}

		rwlayer, err := daemon.layerStore.GetRWLayer(container.ID)
		if err != nil {
			logrus.Errorf("Failed to load container mount %v: %v", id, err)
			continue
		}
		container.RWLayer = rwlayer

		// Ignore the container if it does not support the current driver being used by the graph
		if (container.Driver == "" && currentDriver == "aufs") || container.Driver == currentDriver {
			logrus.Debugf("Loaded container %v", container.ID)

			containers[container.ID] = &cr{container: container}
		} else {
			logrus.Debugf("Cannot load container %s because it was created with another graph driver.", container.ID)
		}
	}

	if entities := daemon.containerGraphDB.List("/", -1); entities != nil {
		for _, p := range entities.Paths() {
			if !debug && logrus.GetLevel() == logrus.InfoLevel {
				fmt.Print(".")
			}

			e := entities[p]

			if c, ok := containers[e.ID()]; ok {
				c.registered = true
			}
		}
	}

	restartContainers := make(map[*container.Container]chan struct{})
	for _, c := range containers {
		if !c.registered {
			// Try to set the default name for a container if it exists prior to links
			c.container.Name, err = daemon.generateNewName(c.container.ID)
			if err != nil {
				logrus.Debugf("Setting default id - %s", err)
			}
			if err := daemon.registerName(c.container); err != nil {
				logrus.Errorf("Failed to register container %s: %s", c.container.ID, err)
				continue
			}
		}

		if err := daemon.Register(c.container); err != nil {
			logrus.Errorf("Failed to register container %s: %s", c.container.ID, err)
			continue
		}
		// get list of containers we need to restart
		if daemon.configStore.AutoRestart && c.container.ShouldRestart() {
			restartContainers[c.container] = make(chan struct{})
		}
	}

	group := sync.WaitGroup{}
	for c, notifier := range restartContainers {
		group.Add(1)
		go func(container *container.Container, chNotify chan struct{}) {
			defer group.Done()
			logrus.Debugf("Starting container %s", container.ID)

			// ignore errors here as this is a best effort to wait for children to be
			//   running before we try to start the container
			children, err := daemon.children(container.Name)
			if err != nil {
				logrus.Warnf("error getting children for %s: %v", container.Name, err)
			}
			timeout := time.After(5 * time.Second)
			for _, child := range children {
				if notifier, exists := restartContainers[child]; exists {
					select {
					case <-notifier:
					case <-timeout:
					}
				}
			}
			if err := daemon.containerStart(container); err != nil {
				logrus.Errorf("Failed to start container %s: %s", container.ID, err)
			}
			close(chNotify)
		}(c, notifier)
	}
	group.Wait()

	if !debug {
		if logrus.GetLevel() == logrus.InfoLevel {
			fmt.Println()
		}
		logrus.Info("Loading containers: done.")
	}

	return nil
}

func (daemon *Daemon) mergeAndVerifyConfig(config *containertypes.Config, img *image.Image) error {
	if img != nil && img.Config != nil {
		if err := runconfig.Merge(config, img.Config); err != nil {
			return err
		}
	}
	if config.Entrypoint.Len() == 0 && config.Cmd.Len() == 0 {
		return fmt.Errorf("No command specified")
	}
	return nil
}

func (daemon *Daemon) generateIDAndName(name string) (string, string, error) {
	var (
		err error
		id  = stringid.GenerateNonCryptoID()
	)

	if name == "" {
		if name, err = daemon.generateNewName(id); err != nil {
			return "", "", err
		}
		return id, name, nil
	}

	if name, err = daemon.reserveName(id, name); err != nil {
		return "", "", err
	}

	return id, name, nil
}

func (daemon *Daemon) reserveName(id, name string) (string, error) {
	if !validContainerNamePattern.MatchString(name) {
		return "", fmt.Errorf("Invalid container name (%s), only %s are allowed", name, validContainerNameChars)
	}

	if name[0] != '/' {
		name = "/" + name
	}

	if _, err := daemon.containerGraphDB.Set(name, id); err != nil {
		if !graphdb.IsNonUniqueNameError(err) {
			return "", err
		}

		conflictingContainer, err := daemon.GetByName(name)
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf(
			"Conflict. The name %q is already in use by container %s. You have to remove (or rename) that container to be able to reuse that name.", strings.TrimPrefix(name, "/"),
			stringid.TruncateID(conflictingContainer.ID))

	}
	return name, nil
}

func (daemon *Daemon) generateNewName(id string) (string, error) {
	var name string
	for i := 0; i < 6; i++ {
		name = namesgenerator.GetRandomName(i)
		if name[0] != '/' {
			name = "/" + name
		}

		if _, err := daemon.containerGraphDB.Set(name, id); err != nil {
			if !graphdb.IsNonUniqueNameError(err) {
				return "", err
			}
			continue
		}
		return name, nil
	}

	name = "/" + stringid.TruncateID(id)
	if _, err := daemon.containerGraphDB.Set(name, id); err != nil {
		return "", err
	}
	return name, nil
}

func (daemon *Daemon) generateHostname(id string, config *containertypes.Config) {
	// Generate default hostname
	if config.Hostname == "" {
		config.Hostname = id[:12]
	}
}

func (daemon *Daemon) getEntrypointAndArgs(configEntrypoint *strslice.StrSlice, configCmd *strslice.StrSlice) (string, []string) {
	cmdSlice := configCmd.Slice()
	if configEntrypoint.Len() != 0 {
		eSlice := configEntrypoint.Slice()
		return eSlice[0], append(eSlice[1:], cmdSlice...)
	}
	return cmdSlice[0], cmdSlice[1:]
}

func (daemon *Daemon) newContainer(name string, config *containertypes.Config, imgID image.ID) (*container.Container, error) {
	var (
		id             string
		err            error
		noExplicitName = name == ""
	)
	id, name, err = daemon.generateIDAndName(name)
	if err != nil {
		return nil, err
	}

	daemon.generateHostname(id, config)
	entrypoint, args := daemon.getEntrypointAndArgs(config.Entrypoint, config.Cmd)

	base := daemon.newBaseContainer(id)
	base.Created = time.Now().UTC()
	base.Path = entrypoint
	base.Args = args //FIXME: de-duplicate from config
	base.Config = config
	base.HostConfig = &containertypes.HostConfig{}
	base.ImageID = imgID
	base.NetworkSettings = &network.Settings{IsAnonymousEndpoint: noExplicitName}
	base.Name = name
	base.Driver = daemon.GraphDriverName()

	return base, err
}

// GetFullContainerName returns a constructed container name. I think
// it has to do with the fact that a container is a file on disk and
// this is sort of just creating a file name.
func GetFullContainerName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("Container name cannot be empty")
	}
	if name[0] != '/' {
		name = "/" + name
	}
	return name, nil
}

// GetByName returns a container given a name.
func (daemon *Daemon) GetByName(name string) (*container.Container, error) {
	fullName, err := GetFullContainerName(name)
	if err != nil {
		return nil, err
	}
	entity := daemon.containerGraphDB.Get(fullName)
	if entity == nil {
		return nil, fmt.Errorf("Could not find entity for %s", name)
	}
	e := daemon.containers.Get(entity.ID())
	if e == nil {
		return nil, fmt.Errorf("Could not find container for entity id %s", entity.ID())
	}
	return e, nil
}

// SubscribeToEvents returns the currently record of events, a channel to stream new events from, and a function to cancel the stream of events.
func (daemon *Daemon) SubscribeToEvents(since, sinceNano int64, filter filters.Args) ([]eventtypes.Message, chan interface{}) {
	ef := events.NewFilter(filter)
	return daemon.EventsService.SubscribeTopic(since, sinceNano, ef)
}

// UnsubscribeFromEvents stops the event subscription for a client by closing the
// channel where the daemon sends events to.
func (daemon *Daemon) UnsubscribeFromEvents(listener chan interface{}) {
	daemon.EventsService.Evict(listener)
}

// children returns all child containers of the container with the
// given name. The containers are returned as a map from the container
// name to a pointer to Container.
func (daemon *Daemon) children(name string) (map[string]*container.Container, error) {
	name, err := GetFullContainerName(name)
	if err != nil {
		return nil, err
	}
	children := make(map[string]*container.Container)

	err = daemon.containerGraphDB.Walk(name, func(p string, e *graphdb.Entity) error {
		c, err := daemon.GetContainer(e.ID())
		if err != nil {
			return err
		}
		children[p] = c
		return nil
	}, 0)

	if err != nil {
		return nil, err
	}
	return children, nil
}

// parents returns the names of the parent containers of the container
// with the given name.
func (daemon *Daemon) parents(name string) ([]string, error) {
	name, err := GetFullContainerName(name)
	if err != nil {
		return nil, err
	}

	return daemon.containerGraphDB.Parents(name)
}

func (daemon *Daemon) registerLink(parent, child *container.Container, alias string) error {
	fullName := filepath.Join(parent.Name, alias)
	if !daemon.containerGraphDB.Exists(fullName) {
		_, err := daemon.containerGraphDB.Set(fullName, child.ID)
		return err
	}
	return nil
}

// NewDaemon sets up everything for the daemon to be able to service
// requests from the webserver.
func NewDaemon(config *Config, registryService *registry.Service) (daemon *Daemon, err error) {
	setDefaultMtu(config)

	// Ensure we have compatible configuration options
	if err := checkConfigOptions(config); err != nil {
		return nil, err
	}

	// Do we have a disabled network?
	config.DisableBridge = isBridgeNetworkDisabled(config)

	// Verify the platform is supported as a daemon
	if !platformSupported {
		return nil, errSystemNotSupported
	}

	// Validate platform-specific requirements
	if err := checkSystem(); err != nil {
		return nil, err
	}

	// set up SIGUSR1 handler on Unix-like systems, or a Win32 global event
	// on Windows to dump Go routine stacks
	setupDumpStackTrap()

	uidMaps, gidMaps, err := setupRemappedRoot(config)
	if err != nil {
		return nil, err
	}
	rootUID, rootGID, err := idtools.GetRootUIDGID(uidMaps, gidMaps)
	if err != nil {
		return nil, err
	}

	// get the canonical path to the Docker root directory
	var realRoot string
	if _, err := os.Stat(config.Root); err != nil && os.IsNotExist(err) {
		realRoot = config.Root
	} else {
		realRoot, err = fileutils.ReadSymlinkedDirectory(config.Root)
		if err != nil {
			return nil, fmt.Errorf("Unable to get the full path to root (%s): %s", config.Root, err)
		}
	}

	if err = setupDaemonRoot(config, realRoot, rootUID, rootGID); err != nil {
		return nil, err
	}

	// set up the tmpDir to use a canonical path
	tmp, err := tempDir(config.Root, rootUID, rootGID)
	if err != nil {
		return nil, fmt.Errorf("Unable to get the TempDir under %s: %s", config.Root, err)
	}
	realTmp, err := fileutils.ReadSymlinkedDirectory(tmp)
	if err != nil {
		return nil, fmt.Errorf("Unable to get the full path to the TempDir (%s): %s", tmp, err)
	}
	os.Setenv("TMPDIR", realTmp)

	d := &Daemon{}
	// Ensure the daemon is properly shutdown if there is a failure during
	// initialization
	defer func() {
		if err != nil {
			if err := d.Shutdown(); err != nil {
				logrus.Error(err)
			}
		}
	}()

	// Verify logging driver type
	if config.LogConfig.Type != "none" {
		if _, err := logger.GetLogDriver(config.LogConfig.Type); err != nil {
			return nil, fmt.Errorf("error finding the logging driver: %v", err)
		}
	}
	logrus.Debugf("Using default logging driver %s", config.LogConfig.Type)

	daemonRepo := filepath.Join(config.Root, "containers")
	if err := idtools.MkdirAllAs(daemonRepo, 0700, rootUID, rootGID); err != nil && !os.IsExist(err) {
		return nil, err
	}

	driverName := os.Getenv("DOCKER_DRIVER")
	if driverName == "" {
		driverName = config.GraphDriver
	}
	d.layerStore, err = layer.NewStoreFromOptions(layer.StoreOptions{
		StorePath:                 config.Root,
		MetadataStorePathTemplate: filepath.Join(config.Root, "image", "%s", "layerdb"),
		GraphDriver:               driverName,
		GraphDriverOptions:        config.GraphOptions,
		UIDMaps:                   uidMaps,
		GIDMaps:                   gidMaps,
	})
	if err != nil {
		return nil, err
	}

	graphDriver := d.layerStore.DriverName()
	imageRoot := filepath.Join(config.Root, "image", graphDriver)

	// Configure and validate the kernels security support
	if err := configureKernelSecuritySupport(config, graphDriver); err != nil {
		return nil, err
	}

	d.downloadManager = xfer.NewLayerDownloadManager(d.layerStore, maxDownloadConcurrency)
	d.uploadManager = xfer.NewLayerUploadManager(maxUploadConcurrency)

	ifs, err := image.NewFSStoreBackend(filepath.Join(imageRoot, "imagedb"))
	if err != nil {
		return nil, err
	}

	d.imageStore, err = image.NewImageStore(ifs, d.layerStore)
	if err != nil {
		return nil, err
	}

	// Configure the volumes driver
	volStore, err := configureVolumes(config, rootUID, rootGID)
	if err != nil {
		return nil, err
	}

	trustKey, err := api.LoadOrCreateTrustKey(config.TrustKeyPath)
	if err != nil {
		return nil, err
	}

	trustDir := filepath.Join(config.Root, "trust")

	if err := system.MkdirAll(trustDir, 0700); err != nil {
		return nil, err
	}

	distributionMetadataStore, err := dmetadata.NewFSMetadataStore(filepath.Join(imageRoot, "distribution"))
	if err != nil {
		return nil, err
	}

	eventsService := events.New()

	referenceStore, err := reference.NewReferenceStore(filepath.Join(imageRoot, "repositories.json"))
	if err != nil {
		return nil, fmt.Errorf("Couldn't create Tag store repositories: %s", err)
	}

	if err := restoreCustomImage(d.imageStore, d.layerStore, referenceStore); err != nil {
		return nil, fmt.Errorf("Couldn't restore custom images: %s", err)
	}

	if err := v1.Migrate(config.Root, graphDriver, d.layerStore, d.imageStore, referenceStore, distributionMetadataStore); err != nil {
		return nil, err
	}

	// Discovery is only enabled when the daemon is launched with an address to advertise.  When
	// initialized, the daemon is registered and we can store the discovery backend as its read-only
	// DiscoveryWatcher version.
	if config.ClusterStore != "" && config.ClusterAdvertise != "" {
		advertise, err := discovery.ParseAdvertise(config.ClusterStore, config.ClusterAdvertise)
		if err != nil {
			return nil, fmt.Errorf("discovery advertise parsing failed (%v)", err)
		}
		config.ClusterAdvertise = advertise
		d.discoveryWatcher, err = initDiscovery(config.ClusterStore, config.ClusterAdvertise, config.ClusterOpts)
		if err != nil {
			return nil, fmt.Errorf("discovery initialization failed (%v)", err)
		}
	} else if config.ClusterAdvertise != "" {
		return nil, fmt.Errorf("invalid cluster configuration. --cluster-advertise must be accompanied by --cluster-store configuration")
	}

	d.netController, err = d.initNetworkController(config)
	if err != nil {
		return nil, fmt.Errorf("Error initializing network controller: %v", err)
	}

	graphdbPath := filepath.Join(config.Root, "linkgraph.db")
	graph, err := graphdb.NewSqliteConn(graphdbPath)
	if err != nil {
		return nil, err
	}

	d.containerGraphDB = graph

	sysInfo := sysinfo.New(false)
	// Check if Devices cgroup is mounted, it is hard requirement for container security,
	// on Linux/FreeBSD.
	if runtime.GOOS != "windows" && !sysInfo.CgroupDevicesEnabled {
		return nil, fmt.Errorf("Devices cgroup isn't mounted")
	}

	ed, err := execdrivers.NewDriver(config.ExecOptions, config.ExecRoot, config.Root, sysInfo)
	if err != nil {
		return nil, err
	}

	d.ID = trustKey.PublicKey().KeyID()
	d.repository = daemonRepo
	d.containers = &contStore{s: make(map[string]*container.Container)}
	d.execCommands = exec.NewStore()
	d.referenceStore = referenceStore
	d.distributionMetadataStore = distributionMetadataStore
	d.trustKey = trustKey
	d.idIndex = truncindex.NewTruncIndex([]string{})
	d.configStore = config
	d.execDriver = ed
	d.statsCollector = d.newStatsCollector(1 * time.Second)
	d.defaultLogConfig = config.LogConfig
	d.RegistryService = registryService
	d.EventsService = eventsService
	d.volumes = volStore
	d.root = config.Root
	d.uidMaps = uidMaps
	d.gidMaps = gidMaps

	if err := d.cleanupMounts(); err != nil {
		return nil, err
	}

	go d.execCommandGC()

	if err := d.restore(); err != nil {
		return nil, err
	}

	return d, nil
}

func (daemon *Daemon) shutdownContainer(c *container.Container) error {
	// TODO(windows): Handle docker restart with paused containers
	if c.IsPaused() {
		// To terminate a process in freezer cgroup, we should send
		// SIGTERM to this process then unfreeze it, and the process will
		// force to terminate immediately.
		logrus.Debugf("Found container %s is paused, sending SIGTERM before unpause it", c.ID)
		sig, ok := signal.SignalMap["TERM"]
		if !ok {
			return fmt.Errorf("System doesn not support SIGTERM")
		}
		if err := daemon.kill(c, int(sig)); err != nil {
			return fmt.Errorf("sending SIGTERM to container %s with error: %v", c.ID, err)
		}
		if err := daemon.containerUnpause(c); err != nil {
			return fmt.Errorf("Failed to unpause container %s with error: %v", c.ID, err)
		}
		if _, err := c.WaitStop(10 * time.Second); err != nil {
			logrus.Debugf("container %s failed to exit in 10 second of SIGTERM, sending SIGKILL to force", c.ID)
			sig, ok := signal.SignalMap["KILL"]
			if !ok {
				return fmt.Errorf("System does not support SIGKILL")
			}
			if err := daemon.kill(c, int(sig)); err != nil {
				logrus.Errorf("Failed to SIGKILL container %s", c.ID)
			}
			c.WaitStop(-1 * time.Second)
			return err
		}
	}
	// If container failed to exit in 10 seconds of SIGTERM, then using the force
	if err := daemon.containerStop(c, 10); err != nil {
		return fmt.Errorf("Stop container %s with error: %v", c.ID, err)
	}

	c.WaitStop(-1 * time.Second)
	return nil
}

// Shutdown stops the daemon.
func (daemon *Daemon) Shutdown() error {
	daemon.shutdown = true
	if daemon.containers != nil {
		group := sync.WaitGroup{}
		logrus.Debug("starting clean shutdown of all containers...")
		for _, cont := range daemon.List() {
			if !cont.IsRunning() {
				continue
			}
			logrus.Debugf("stopping %s", cont.ID)
			group.Add(1)
			go func(c *container.Container) {
				defer group.Done()
				if err := daemon.shutdownContainer(c); err != nil {
					logrus.Errorf("Stop container error: %v", err)
					return
				}
				logrus.Debugf("container stopped %s", c.ID)
			}(cont)
		}
		group.Wait()
	}

	// trigger libnetwork Stop only if it's initialized
	if daemon.netController != nil {
		daemon.netController.Stop()
	}

	if daemon.containerGraphDB != nil {
		if err := daemon.containerGraphDB.Close(); err != nil {
			logrus.Errorf("Error during container graph.Close(): %v", err)
		}
	}

	if daemon.layerStore != nil {
		if err := daemon.layerStore.Cleanup(); err != nil {
			logrus.Errorf("Error during layer Store.Cleanup(): %v", err)
		}
	}

	if err := daemon.cleanupMounts(); err != nil {
		return err
	}

	return nil
}

// Mount sets container.BaseFS
// (is it not set coming in? why is it unset?)
func (daemon *Daemon) Mount(container *container.Container) error {
	dir, err := container.RWLayer.Mount(container.GetMountLabel())
	if err != nil {
		return err
	}
	logrus.Debugf("container mounted via layerStore: %v", dir)

	if container.BaseFS != dir {
		// The mount path reported by the graph driver should always be trusted on Windows, since the
		// volume path for a given mounted layer may change over time.  This should only be an error
		// on non-Windows operating systems.
		if container.BaseFS != "" && runtime.GOOS != "windows" {
			daemon.Unmount(container)
			return fmt.Errorf("Error: driver %s is returning inconsistent paths for container %s ('%s' then '%s')",
				daemon.GraphDriverName(), container.ID, container.BaseFS, dir)
		}
	}
	container.BaseFS = dir // TODO: combine these fields
	return nil
}

// Unmount unsets the container base filesystem
func (daemon *Daemon) Unmount(container *container.Container) {
	if err := container.RWLayer.Unmount(); err != nil {
		logrus.Errorf("Error unmounting container %s: %s", container.ID, err)
	}
}

// Run uses the execution driver to run a given container
func (daemon *Daemon) Run(c *container.Container, pipes *execdriver.Pipes, startCallback execdriver.DriverCallback) (execdriver.ExitStatus, error) {
	hooks := execdriver.Hooks{
		Start: startCallback,
	}
	hooks.PreStart = append(hooks.PreStart, func(processConfig *execdriver.ProcessConfig, pid int, chOOM <-chan struct{}) error {
		return daemon.setNetworkNamespaceKey(c.ID, pid)
	})
	return daemon.execDriver.Run(c.Command, pipes, hooks)
}

func (daemon *Daemon) kill(c *container.Container, sig int) error {
	return daemon.execDriver.Kill(c.Command, sig)
}

func (daemon *Daemon) stats(c *container.Container) (*execdriver.ResourceStats, error) {
	return daemon.execDriver.Stats(c.ID)
}

func (daemon *Daemon) subscribeToContainerStats(c *container.Container) chan interface{} {
	return daemon.statsCollector.collect(c)
}

func (daemon *Daemon) unsubscribeToContainerStats(c *container.Container, ch chan interface{}) {
	daemon.statsCollector.unsubscribe(c, ch)
}

func (daemon *Daemon) changes(container *container.Container) ([]archive.Change, error) {
	return container.RWLayer.Changes()
}

// TagImage creates a tag in the repository reponame, pointing to the image named
// imageName.
func (daemon *Daemon) TagImage(newTag reference.Named, imageName string) error {
	imageID, err := daemon.GetImageID(imageName)
	if err != nil {
		return err
	}
	if err := daemon.referenceStore.AddTag(newTag, imageID, true); err != nil {
		return err
	}

	daemon.LogImageEvent(imageID.String(), newTag.String(), "tag")
	return nil
}

func writeDistributionProgress(cancelFunc func(), outStream io.Writer, progressChan <-chan progress.Progress) {
	progressOutput := streamformatter.NewJSONStreamFormatter().NewProgressOutput(outStream, false)
	operationCancelled := false

	for prog := range progressChan {
		if err := progressOutput.WriteProgress(prog); err != nil && !operationCancelled {
			logrus.Errorf("error writing progress to client: %v", err)
			cancelFunc()
			operationCancelled = true
			// Don't return, because we need to continue draining
			// progressChan until it's closed to avoid a deadlock.
		}
	}
}

// PullImage initiates a pull operation. image is the repository name to pull, and
// tag may be either empty, or indicate a specific tag to pull.
func (daemon *Daemon) PullImage(ref reference.Named, metaHeaders map[string][]string, authConfig *types.AuthConfig, outStream io.Writer) error {
	// Include a buffer so that slow client connections don't affect
	// transfer performance.
	progressChan := make(chan progress.Progress, 100)

	writesDone := make(chan struct{})

	ctx, cancelFunc := context.WithCancel(context.Background())

	go func() {
		writeDistributionProgress(cancelFunc, outStream, progressChan)
		close(writesDone)
	}()

	imagePullConfig := &distribution.ImagePullConfig{
		MetaHeaders:      metaHeaders,
		AuthConfig:       authConfig,
		ProgressOutput:   progress.ChanOutput(progressChan),
		RegistryService:  daemon.RegistryService,
		ImageEventLogger: daemon.LogImageEvent,
		MetadataStore:    daemon.distributionMetadataStore,
		ImageStore:       daemon.imageStore,
		ReferenceStore:   daemon.referenceStore,
		DownloadManager:  daemon.downloadManager,
	}

	err := distribution.Pull(ctx, ref, imagePullConfig)
	close(progressChan)
	<-writesDone
	return err
}

// ExportImage exports a list of images to the given output stream. The
// exported images are archived into a tar when written to the output
// stream. All images with the given tag and all versions containing
// the same tag are exported. names is the set of tags to export, and
// outStream is the writer which the images are written to.
func (daemon *Daemon) ExportImage(names []string, outStream io.Writer) error {
	imageExporter := tarexport.NewTarExporter(daemon.imageStore, daemon.layerStore, daemon.referenceStore)
	return imageExporter.Save(names, outStream)
}

// PushImage initiates a push operation on the repository named localName.
func (daemon *Daemon) PushImage(ref reference.Named, metaHeaders map[string][]string, authConfig *types.AuthConfig, outStream io.Writer) error {
	// Include a buffer so that slow client connections don't affect
	// transfer performance.
	progressChan := make(chan progress.Progress, 100)

	writesDone := make(chan struct{})

	ctx, cancelFunc := context.WithCancel(context.Background())

	go func() {
		writeDistributionProgress(cancelFunc, outStream, progressChan)
		close(writesDone)
	}()

	imagePushConfig := &distribution.ImagePushConfig{
		MetaHeaders:      metaHeaders,
		AuthConfig:       authConfig,
		ProgressOutput:   progress.ChanOutput(progressChan),
		RegistryService:  daemon.RegistryService,
		ImageEventLogger: daemon.LogImageEvent,
		MetadataStore:    daemon.distributionMetadataStore,
		LayerStore:       daemon.layerStore,
		ImageStore:       daemon.imageStore,
		ReferenceStore:   daemon.referenceStore,
		TrustKey:         daemon.trustKey,
		UploadManager:    daemon.uploadManager,
	}

	err := distribution.Push(ctx, ref, imagePushConfig)
	close(progressChan)
	<-writesDone
	return err
}

// LookupImage looks up an image by name and returns it as an ImageInspect
// structure.
func (daemon *Daemon) LookupImage(name string) (*types.ImageInspect, error) {
	img, err := daemon.GetImage(name)
	if err != nil {
		return nil, fmt.Errorf("No such image: %s", name)
	}

	refs := daemon.referenceStore.References(img.ID())
	repoTags := []string{}
	repoDigests := []string{}
	for _, ref := range refs {
		switch ref.(type) {
		case reference.NamedTagged:
			repoTags = append(repoTags, ref.String())
		case reference.Canonical:
			repoDigests = append(repoDigests, ref.String())
		}
	}

	var size int64
	var layerMetadata map[string]string
	layerID := img.RootFS.ChainID()
	if layerID != "" {
		l, err := daemon.layerStore.Get(layerID)
		if err != nil {
			return nil, err
		}
		defer layer.ReleaseAndLog(daemon.layerStore, l)
		size, err = l.Size()
		if err != nil {
			return nil, err
		}

		layerMetadata, err = l.Metadata()
		if err != nil {
			return nil, err
		}
	}

	comment := img.Comment
	if len(comment) == 0 && len(img.History) > 0 {
		comment = img.History[len(img.History)-1].Comment
	}

	imageInspect := &types.ImageInspect{
		ID:              img.ID().String(),
		RepoTags:        repoTags,
		RepoDigests:     repoDigests,
		Parent:          img.Parent.String(),
		Comment:         comment,
		Created:         img.Created.Format(time.RFC3339Nano),
		Container:       img.Container,
		ContainerConfig: &img.ContainerConfig,
		DockerVersion:   img.DockerVersion,
		Author:          img.Author,
		Config:          img.Config,
		Architecture:    img.Architecture,
		Os:              img.OS,
		Size:            size,
		VirtualSize:     size, // TODO: field unused, deprecate
	}

	imageInspect.GraphDriver.Name = daemon.GraphDriverName()

	imageInspect.GraphDriver.Data = layerMetadata

	return imageInspect, nil
}

// LoadImage uploads a set of images into the repository. This is the
// complement of ImageExport.  The input stream is an uncompressed tar
// ball containing images and metadata.
func (daemon *Daemon) LoadImage(inTar io.ReadCloser, outStream io.Writer) error {
	imageExporter := tarexport.NewTarExporter(daemon.imageStore, daemon.layerStore, daemon.referenceStore)
	return imageExporter.Load(inTar, outStream)
}

// ImageHistory returns a slice of ImageHistory structures for the specified image
// name by walking the image lineage.
func (daemon *Daemon) ImageHistory(name string) ([]*types.ImageHistory, error) {
	img, err := daemon.GetImage(name)
	if err != nil {
		return nil, err
	}

	history := []*types.ImageHistory{}

	layerCounter := 0
	rootFS := *img.RootFS
	rootFS.DiffIDs = nil

	for _, h := range img.History {
		var layerSize int64

		if !h.EmptyLayer {
			if len(img.RootFS.DiffIDs) <= layerCounter {
				return nil, errors.New("too many non-empty layers in History section")
			}

			rootFS.Append(img.RootFS.DiffIDs[layerCounter])
			l, err := daemon.layerStore.Get(rootFS.ChainID())
			if err != nil {
				return nil, err
			}
			layerSize, err = l.DiffSize()
			layer.ReleaseAndLog(daemon.layerStore, l)
			if err != nil {
				return nil, err
			}

			layerCounter++
		}

		history = append([]*types.ImageHistory{{
			ID:        "<missing>",
			Created:   h.Created.Unix(),
			CreatedBy: h.CreatedBy,
			Comment:   h.Comment,
			Size:      layerSize,
		}}, history...)
	}

	// Fill in image IDs and tags
	histImg := img
	id := img.ID()
	for _, h := range history {
		h.ID = id.String()

		var tags []string
		for _, r := range daemon.referenceStore.References(id) {
			if _, ok := r.(reference.NamedTagged); ok {
				tags = append(tags, r.String())
			}
		}

		h.Tags = tags

		id = histImg.Parent
		if id == "" {
			break
		}
		histImg, err = daemon.GetImage(id.String())
		if err != nil {
			break
		}
	}

	return history, nil
}

// GetImageID returns an image ID corresponding to the image referred to by
// refOrID.
func (daemon *Daemon) GetImageID(refOrID string) (image.ID, error) {
	// Treat as an ID
	if id, err := digest.ParseDigest(refOrID); err == nil {
		return image.ID(id), nil
	}

	// Treat it as a possible tag or digest reference
	if ref, err := reference.ParseNamed(refOrID); err == nil {
		if id, err := daemon.referenceStore.Get(ref); err == nil {
			return id, nil
		}
		if tagged, ok := ref.(reference.NamedTagged); ok {
			if id, err := daemon.imageStore.Search(tagged.Tag()); err == nil {
				for _, namedRef := range daemon.referenceStore.References(id) {
					if namedRef.Name() == ref.Name() {
						return id, nil
					}
				}
			}
		}
	}

	// Search based on ID
	if id, err := daemon.imageStore.Search(refOrID); err == nil {
		return id, nil
	}

	return "", ErrImageDoesNotExist{refOrID}
}

// GetImage returns an image corresponding to the image referred to by refOrID.
func (daemon *Daemon) GetImage(refOrID string) (*image.Image, error) {
	imgID, err := daemon.GetImageID(refOrID)
	if err != nil {
		return nil, err
	}
	return daemon.imageStore.Get(imgID)
}

// GraphDriverName returns the name of the graph driver used by the layer.Store
func (daemon *Daemon) GraphDriverName() string {
	return daemon.layerStore.DriverName()
}

// ExecutionDriver returns the currently used driver for creating and
// starting execs in a container.
func (daemon *Daemon) ExecutionDriver() execdriver.Driver {
	return daemon.execDriver
}

func (daemon *Daemon) containerGraph() *graphdb.Database {
	return daemon.containerGraphDB
}

// GetUIDGIDMaps returns the current daemon's user namespace settings
// for the full uid and gid maps which will be applied to containers
// started in this instance.
func (daemon *Daemon) GetUIDGIDMaps() ([]idtools.IDMap, []idtools.IDMap) {
	return daemon.uidMaps, daemon.gidMaps
}

// GetRemappedUIDGID returns the current daemon's uid and gid values
// if user namespaces are in use for this daemon instance.  If not
// this function will return "real" root values of 0, 0.
func (daemon *Daemon) GetRemappedUIDGID() (int, int) {
	uid, gid, _ := idtools.GetRootUIDGID(daemon.uidMaps, daemon.gidMaps)
	return uid, gid
}

// ImageGetCached returns the earliest created image that is a child
// of the image with imgID, that had the same config when it was
// created. nil is returned if a child cannot be found. An error is
// returned if the parent image cannot be found.
func (daemon *Daemon) ImageGetCached(imgID image.ID, config *containertypes.Config) (*image.Image, error) {
	// Retrieve all images
	imgs := daemon.Map()

	var siblings []image.ID
	for id, img := range imgs {
		if img.Parent == imgID {
			siblings = append(siblings, id)
		}
	}

	// Loop on the children of the given image and check the config
	var match *image.Image
	for _, id := range siblings {
		img, ok := imgs[id]
		if !ok {
			return nil, fmt.Errorf("unable to find image %q", id)
		}
		if runconfig.Compare(&img.ContainerConfig, config) {
			if match == nil || match.Created.Before(img.Created) {
				match = img
			}
		}
	}
	return match, nil
}

// tempDir returns the default directory to use for temporary files.
func tempDir(rootDir string, rootUID, rootGID int) (string, error) {
	var tmpDir string
	if tmpDir = os.Getenv("DOCKER_TMPDIR"); tmpDir == "" {
		tmpDir = filepath.Join(rootDir, "tmp")
	}
	return tmpDir, idtools.MkdirAllAs(tmpDir, 0700, rootUID, rootGID)
}

func (daemon *Daemon) setSecurityOptions(container *container.Container, hostConfig *containertypes.HostConfig) error {
	container.Lock()
	defer container.Unlock()
	return parseSecurityOpt(container, hostConfig)
}

func (daemon *Daemon) setHostConfig(container *container.Container, hostConfig *containertypes.HostConfig) error {
	// Do not lock while creating volumes since this could be calling out to external plugins
	// Don't want to block other actions, like `docker ps` because we're waiting on an external plugin
	if err := daemon.registerMountPoints(container, hostConfig); err != nil {
		return err
	}

	container.Lock()
	defer container.Unlock()

	// Register any links from the host config before starting the container
	if err := daemon.registerLinks(container, hostConfig); err != nil {
		return err
	}

	container.HostConfig = hostConfig
	container.ToDisk()
	return nil
}

func (daemon *Daemon) setupInitLayer(initPath string) error {
	rootUID, rootGID := daemon.GetRemappedUIDGID()
	return setupInitLayer(initPath, rootUID, rootGID)
}

func setDefaultMtu(config *Config) {
	// do nothing if the config does not have the default 0 value.
	if config.Mtu != 0 {
		return
	}
	config.Mtu = defaultNetworkMtu
}

// verifyContainerSettings performs validation of the hostconfig and config
// structures.
func (daemon *Daemon) verifyContainerSettings(hostConfig *containertypes.HostConfig, config *containertypes.Config) ([]string, error) {

	// First perform verification of settings common across all platforms.
	if config != nil {
		if config.WorkingDir != "" {
			config.WorkingDir = filepath.FromSlash(config.WorkingDir) // Ensure in platform semantics
			if !system.IsAbs(config.WorkingDir) {
				return nil, fmt.Errorf("The working directory '%s' is invalid. It needs to be an absolute path.", config.WorkingDir)
			}
		}

		if len(config.StopSignal) > 0 {
			_, err := signal.ParseSignal(config.StopSignal)
			if err != nil {
				return nil, err
			}
		}
	}

	if hostConfig == nil {
		return nil, nil
	}

	for port := range hostConfig.PortBindings {
		_, portStr := nat.SplitProtoPort(string(port))
		if _, err := nat.ParsePort(portStr); err != nil {
			return nil, fmt.Errorf("Invalid port specification: %q", portStr)
		}
		for _, pb := range hostConfig.PortBindings[port] {
			_, err := nat.NewPort(nat.SplitProtoPort(pb.HostPort))
			if err != nil {
				return nil, fmt.Errorf("Invalid port specification: %q", pb.HostPort)
			}
		}
	}

	// Now do platform-specific verification
	return verifyPlatformContainerSettings(daemon, hostConfig, config)
}

func configureVolumes(config *Config, rootUID, rootGID int) (*store.VolumeStore, error) {
	volumesDriver, err := local.New(config.Root, rootUID, rootGID)
	if err != nil {
		return nil, err
	}

	volumedrivers.Register(volumesDriver, volumesDriver.Name())
	s := store.New()
	s.AddAll(volumesDriver.List())

	return s, nil
}

// AuthenticateToRegistry checks the validity of credentials in authConfig
func (daemon *Daemon) AuthenticateToRegistry(authConfig *types.AuthConfig) (string, error) {
	return daemon.RegistryService.Auth(authConfig)
}

// SearchRegistryForImages queries the registry for images matching
// term. authConfig is used to login.
func (daemon *Daemon) SearchRegistryForImages(term string,
	authConfig *types.AuthConfig,
	headers map[string][]string) (*registrytypes.SearchResults, error) {
	return daemon.RegistryService.Search(term, authConfig, headers)
}

// IsShuttingDown tells whether the daemon is shutting down or not
func (daemon *Daemon) IsShuttingDown() bool {
	return daemon.shutdown
}

// GetContainerStats collects all the stats published by a container
func (daemon *Daemon) GetContainerStats(container *container.Container) (*execdriver.ResourceStats, error) {
	stats, err := daemon.stats(container)
	if err != nil {
		return nil, err
	}

	// Retrieve the nw statistics from libnetwork and inject them in the Stats
	var nwStats []*libcontainer.NetworkInterface
	if nwStats, err = daemon.getNetworkStats(container); err != nil {
		return nil, err
	}
	stats.Interfaces = nwStats

	return stats, nil
}

func (daemon *Daemon) getNetworkStats(c *container.Container) ([]*libcontainer.NetworkInterface, error) {
	var list []*libcontainer.NetworkInterface

	sb, err := daemon.netController.SandboxByID(c.NetworkSettings.SandboxID)
	if err != nil {
		return list, err
	}

	stats, err := sb.Statistics()
	if err != nil {
		return list, err
	}

	// Convert libnetwork nw stats into libcontainer nw stats
	for ifName, ifStats := range stats {
		list = append(list, convertLnNetworkStats(ifName, ifStats))
	}

	return list, nil
}

// newBaseContainer creates a new container with its initial
// configuration based on the root storage from the daemon.
func (daemon *Daemon) newBaseContainer(id string) *container.Container {
	return container.NewBaseContainer(id, daemon.containerRoot(id))
}

func convertLnNetworkStats(name string, stats *lntypes.InterfaceStatistics) *libcontainer.NetworkInterface {
	n := &libcontainer.NetworkInterface{Name: name}
	n.RxBytes = stats.RxBytes
	n.RxPackets = stats.RxPackets
	n.RxErrors = stats.RxErrors
	n.RxDropped = stats.RxDropped
	n.TxBytes = stats.TxBytes
	n.TxPackets = stats.TxPackets
	n.TxErrors = stats.TxErrors
	n.TxDropped = stats.TxDropped
	return n
}

func validateID(id string) error {
	if id == "" {
		return derr.ErrorCodeEmptyID
	}
	return nil
}
