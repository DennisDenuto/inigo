package inigo_test

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/config"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	"github.com/pivotal-golang/lager/ginkgoreporter"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
	"github.com/tedsuo/ifrit/grouper"

	garden_api "github.com/cloudfoundry-incubator/garden/api"
	"github.com/cloudfoundry-incubator/inigo/helpers"
	"github.com/cloudfoundry-incubator/inigo/inigo_announcement_server"
	"github.com/cloudfoundry-incubator/inigo/world"
	Bbs "github.com/cloudfoundry-incubator/runtime-schema/bbs"
	"github.com/cloudfoundry/gunk/diegonats"
	"github.com/cloudfoundry/gunk/timeprovider"
	"github.com/cloudfoundry/gunk/workpool"
	"github.com/cloudfoundry/storeadapter/etcdstoreadapter"
)

var DEFAULT_EVENTUALLY_TIMEOUT = 1 * time.Minute
var DEFAULT_CONSISTENTLY_DURATION = 5 * time.Second

// use this for tests exercising docker; pulling can take a while
const DOCKER_PULL_ESTIMATE = 5 * time.Minute

const StackName = "lucid64"

var builtArtifacts world.BuiltArtifacts
var componentMaker world.ComponentMaker

var (
	plumbing      ifrit.Process
	gardenProcess ifrit.Process
	bbs           *Bbs.BBS
	natsClient    diegonats.NATSClient
	gardenClient  garden_api.Client
)

var _ = SynchronizedBeforeSuite(func() []byte {
	payload, err := json.Marshal(world.BuiltArtifacts{
		Executables:  CompileTestedExecutables(),
		Circuses:     CompileAndZipUpCircuses(),
		DockerCircus: CompileAndZipUpDockerCircus(),
	})
	Ω(err).ShouldNot(HaveOccurred())

	return payload
}, func(encodedBuiltArtifacts []byte) {
	var err error

	err = json.Unmarshal(encodedBuiltArtifacts, &builtArtifacts)
	Ω(err).ShouldNot(HaveOccurred())

	addresses := world.ComponentAddresses{
		GardenLinux:    fmt.Sprintf("127.0.0.1:%d", 10000+config.GinkgoConfig.ParallelNode),
		NATS:           fmt.Sprintf("127.0.0.1:%d", 11000+config.GinkgoConfig.ParallelNode),
		Etcd:           fmt.Sprintf("127.0.0.1:%d", 12000+config.GinkgoConfig.ParallelNode),
		EtcdPeer:       fmt.Sprintf("127.0.0.1:%d", 12500+config.GinkgoConfig.ParallelNode),
		Executor:       fmt.Sprintf("127.0.0.1:%d", 13000+config.GinkgoConfig.ParallelNode),
		LoggregatorIn:  fmt.Sprintf("127.0.0.1:%d", 15000+config.GinkgoConfig.ParallelNode),
		LoggregatorOut: fmt.Sprintf("127.0.0.1:%d", 16000+config.GinkgoConfig.ParallelNode),
		FileServer:     fmt.Sprintf("127.0.0.1:%d", 17000+config.GinkgoConfig.ParallelNode),
		Router:         fmt.Sprintf("127.0.0.1:%d", 18000+config.GinkgoConfig.ParallelNode),
		TPS:            fmt.Sprintf("127.0.0.1:%d", 19000+config.GinkgoConfig.ParallelNode),
		FakeCC:         fmt.Sprintf("127.0.0.1:%d", 20000+config.GinkgoConfig.ParallelNode),
		Receptor:       fmt.Sprintf("127.0.0.1:%d", 21000+config.GinkgoConfig.ParallelNode),
		Stager:         fmt.Sprintf("127.0.0.1:%d", 22000+config.GinkgoConfig.ParallelNode),
	}

	gardenBinPath := os.Getenv("GARDEN_BINPATH")
	gardenRootFSPath := os.Getenv("GARDEN_ROOTFS")
	gardenGraphPath := os.Getenv("GARDEN_GRAPH_PATH")
	externalAddress := os.Getenv("EXTERNAL_ADDRESS")

	if gardenGraphPath == "" {
		gardenGraphPath = os.TempDir()
	}

	Ω(gardenBinPath).ShouldNot(BeEmpty(), "must provide $GARDEN_BINPATH")
	Ω(gardenRootFSPath).ShouldNot(BeEmpty(), "must provide $GARDEN_ROOTFS")
	Ω(externalAddress).ShouldNot(BeEmpty(), "must provide $EXTERNAL_ADDRESS")

	componentMaker = world.ComponentMaker{
		Artifacts: builtArtifacts,
		Addresses: addresses,

		Stack: StackName,

		ExternalAddress: externalAddress,

		GardenBinPath:    gardenBinPath,
		GardenRootFSPath: gardenRootFSPath,
		GardenGraphPath:  gardenGraphPath,
	}
})

var _ = BeforeEach(func() {
	currentTestDescription := CurrentGinkgoTestDescription()
	fmt.Fprintf(GinkgoWriter, "\n%s\n%s\n\n", strings.Repeat("~", 50), currentTestDescription.FullTestText)

	gardenLinux := componentMaker.GardenLinux()

	plumbing = ginkgomon.Invoke(grouper.NewParallel(os.Kill, grouper.Members{
		{"etcd", componentMaker.Etcd()},
		{"nats", componentMaker.NATS()},
	}))

	gardenProcess = ginkgomon.Invoke(gardenLinux)

	gardenClient = gardenLinux.NewClient()

	var err error
	natsClient = diegonats.NewClient()
	_, err = natsClient.Connect([]string{"nats://" + componentMaker.Addresses.NATS})
	Ω(err).ShouldNot(HaveOccurred())

	adapter := etcdstoreadapter.NewETCDStoreAdapter([]string{"http://" + componentMaker.Addresses.Etcd}, workpool.NewWorkPool(20))

	err = adapter.Connect()
	Ω(err).ShouldNot(HaveOccurred())

	bbs = Bbs.NewBBS(adapter, timeprovider.NewTimeProvider(), lagertest.NewTestLogger("test"))

	inigo_announcement_server.Start(componentMaker.ExternalAddress)
})

var _ = AfterEach(func() {
	inigo_announcement_server.Stop(gardenClient)

	helpers.StopProcess(plumbing)

	containers, err := gardenClient.Containers(nil)
	Ω(err).ShouldNot(HaveOccurred())

	// even if containers fail to destroy, stop garden, but still report the
	// errors
	destroyContainerErrors := []error{}
	for _, container := range containers {
		err := gardenClient.Destroy(container.Handle())
		if err != nil {
			destroyContainerErrors = append(destroyContainerErrors, err)
		}
	}

	helpers.StopProcess(gardenProcess)

	Ω(destroyContainerErrors).Should(
		BeEmpty(),
		"%d of %d containers failed to be destroyed!",
		len(destroyContainerErrors),
		len(containers),
	)
})

func TestInigo(t *testing.T) {
	registerDefaultTimeouts()

	RegisterFailHandler(Fail)

	RunSpecsWithDefaultAndCustomReporters(t, "Inigo Integration Suite", []Reporter{
		ginkgoreporter.New(GinkgoWriter),
	})
}

func registerDefaultTimeouts() {
	var err error
	if os.Getenv("DEFAULT_EVENTUALLY_TIMEOUT") != "" {
		DEFAULT_EVENTUALLY_TIMEOUT, err = time.ParseDuration(os.Getenv("DEFAULT_EVENTUALLY_TIMEOUT"))
		if err != nil {
			panic(err)
		}
	}

	if os.Getenv("DEFAULT_CONSISTENTLY_DURATION") != "" {
		DEFAULT_CONSISTENTLY_DURATION, err = time.ParseDuration(os.Getenv("DEFAULT_CONSISTENTLY_DURATION"))
		if err != nil {
			panic(err)
		}
	}

	SetDefaultEventuallyTimeout(DEFAULT_EVENTUALLY_TIMEOUT)
	SetDefaultConsistentlyDuration(DEFAULT_CONSISTENTLY_DURATION)

	// most things hit some component; don't hammer it
	SetDefaultConsistentlyPollingInterval(100 * time.Millisecond)
	SetDefaultEventuallyPollingInterval(500 * time.Millisecond)
}

func CompileTestedExecutables() world.BuiltExecutables {
	var err error

	builtExecutables := world.BuiltExecutables{}

	builtExecutables["garden-linux"], err = gexec.BuildIn(os.Getenv("GARDEN_LINUX_GOPATH"), "github.com/cloudfoundry-incubator/garden-linux", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	builtExecutables["loggregator"], err = gexec.BuildIn(os.Getenv("LOGGREGATOR_GOPATH"), "loggregator")
	Ω(err).ShouldNot(HaveOccurred())

	builtExecutables["auctioneer"], err = gexec.BuildIn(os.Getenv("AUCTIONEER_GOPATH"), "github.com/cloudfoundry-incubator/auctioneer/cmd/auctioneer", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	builtExecutables["exec"], err = gexec.BuildIn(os.Getenv("EXECUTOR_GOPATH"), "github.com/cloudfoundry-incubator/executor/cmd/executor", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	builtExecutables["converger"], err = gexec.BuildIn(os.Getenv("CONVERGER_GOPATH"), "github.com/cloudfoundry-incubator/converger/cmd/converger", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	builtExecutables["rep"], err = gexec.BuildIn(os.Getenv("REP_GOPATH"), "github.com/cloudfoundry-incubator/rep/cmd/rep", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	builtExecutables["stager"], err = gexec.BuildIn(os.Getenv("STAGER_GOPATH"), "github.com/cloudfoundry-incubator/stager/cmd/stager", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	builtExecutables["receptor"], err = gexec.BuildIn(os.Getenv("RECEPTOR_GOPATH"), "github.com/cloudfoundry-incubator/receptor/cmd/receptor", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	builtExecutables["nsync-listener"], err = gexec.BuildIn(os.Getenv("NSYNC_GOPATH"), "github.com/cloudfoundry-incubator/nsync/cmd/nsync-listener", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	builtExecutables["nsync-bulker"], err = gexec.BuildIn(os.Getenv("NSYNC_GOPATH"), "github.com/cloudfoundry-incubator/nsync/cmd/nsync-bulker", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	builtExecutables["file-server"], err = gexec.BuildIn(os.Getenv("FILE_SERVER_GOPATH"), "github.com/cloudfoundry-incubator/file-server/cmd/file-server", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	builtExecutables["route-emitter"], err = gexec.BuildIn(os.Getenv("ROUTE_EMITTER_GOPATH"), "github.com/cloudfoundry-incubator/route-emitter/cmd/route-emitter", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	builtExecutables["tps"], err = gexec.BuildIn(os.Getenv("TPS_GOPATH"), "github.com/cloudfoundry-incubator/tps/cmd/tps", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	builtExecutables["router"], err = gexec.BuildIn(os.Getenv("ROUTER_GOPATH"), "github.com/cloudfoundry/gorouter", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	return builtExecutables
}

func CompileAndZipUpCircuses() world.BuiltCircuses {
	builtCircuses := world.BuiltCircuses{}

	tailorPath, err := gexec.BuildIn(os.Getenv("LINUX_CIRCUS_GOPATH"), "github.com/cloudfoundry-incubator/linux-circus/tailor", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	spyPath, err := gexec.BuildIn(os.Getenv("LINUX_CIRCUS_GOPATH"), "github.com/cloudfoundry-incubator/linux-circus/spy", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	soldierPath, err := gexec.BuildIn(os.Getenv("LINUX_CIRCUS_GOPATH"), "github.com/cloudfoundry-incubator/linux-circus/soldier", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	circusDir, err := ioutil.TempDir("", "circus-dir")
	Ω(err).ShouldNot(HaveOccurred())

	err = os.Rename(tailorPath, filepath.Join(circusDir, "tailor"))
	Ω(err).ShouldNot(HaveOccurred())

	err = os.Rename(spyPath, filepath.Join(circusDir, "spy"))
	Ω(err).ShouldNot(HaveOccurred())

	err = os.Rename(soldierPath, filepath.Join(circusDir, "soldier"))
	Ω(err).ShouldNot(HaveOccurred())

	cmd := exec.Command("zip", "-v", "circus.zip", "tailor", "soldier", "spy")
	cmd.Stderr = GinkgoWriter
	cmd.Stdout = GinkgoWriter
	cmd.Dir = circusDir
	err = cmd.Run()
	Ω(err).ShouldNot(HaveOccurred())

	builtCircuses[StackName] = filepath.Join(circusDir, "circus.zip")

	return builtCircuses
}

func CompileAndZipUpDockerCircus() string {
	tailorPath, err := gexec.BuildIn(os.Getenv("DOCKER_CIRCUS_GOPATH"), "github.com/cloudfoundry-incubator/docker-circus/tailor", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	spyPath, err := gexec.BuildIn(os.Getenv("DOCKER_CIRCUS_GOPATH"), "github.com/cloudfoundry-incubator/docker-circus/spy", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	soldierPath, err := gexec.BuildIn(os.Getenv("DOCKER_CIRCUS_GOPATH"), "github.com/cloudfoundry-incubator/docker-circus/soldier", "-race")
	Ω(err).ShouldNot(HaveOccurred())

	circusDir, err := ioutil.TempDir("", "circus-dir")
	Ω(err).ShouldNot(HaveOccurred())

	err = os.Rename(tailorPath, filepath.Join(circusDir, "tailor"))
	Ω(err).ShouldNot(HaveOccurred())

	err = os.Rename(spyPath, filepath.Join(circusDir, "spy"))
	Ω(err).ShouldNot(HaveOccurred())

	err = os.Rename(soldierPath, filepath.Join(circusDir, "soldier"))
	Ω(err).ShouldNot(HaveOccurred())

	cmd := exec.Command("zip", "-v", "docker-circus.zip", "tailor", "soldier", "spy")
	cmd.Stderr = GinkgoWriter
	cmd.Stdout = GinkgoWriter
	cmd.Dir = circusDir
	err = cmd.Run()
	Ω(err).ShouldNot(HaveOccurred())

	return filepath.Join(circusDir, "docker-circus.zip")
}
