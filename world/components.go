package world

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/candiedyaml"
	gardenrunner "github.com/cloudfoundry-incubator/garden-linux/integration/runner"
	"github.com/cloudfoundry-incubator/inigo/fake_cc"
	gorouterconfig "github.com/cloudfoundry/gorouter/config"
	"github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
)

type BuiltExecutables map[string]string
type BuiltCircuses map[string]string

const CircusFilename = "some-circus.zip"
const DockerCircusFilename = "docker-circus.zip"

type BuiltArtifacts struct {
	Executables  BuiltExecutables
	Circuses     BuiltCircuses
	DockerCircus string
}

type ComponentAddresses struct {
	NATS           string
	Etcd           string
	EtcdPeer       string
	LoggregatorIn  string
	LoggregatorOut string
	Executor       string
	FakeCC         string
	FileServer     string
	Router         string
	TPS            string
	GardenLinux    string
	Receptor       string
	Stager         string
}

type ComponentMaker struct {
	Artifacts BuiltArtifacts
	Addresses ComponentAddresses

	ExternalAddress string

	Stack string

	GardenBinPath    string
	GardenRootFSPath string
	GardenGraphPath  string
}

type LoggregatorConfig struct {
	LegacyIncomingMessagesPort int
	OutgoingPort               int
	WSMessageBufferSize        int
	MaxRetainedLogMessages     int
	SharedSecret               string

	NatsHost string
	NatsPort int

	InactivityDurationInMilliseconds int
}

func (maker ComponentMaker) NATS(argv ...string) ifrit.Runner {
	host, port, err := net.SplitHostPort(maker.Addresses.NATS)
	Ω(err).ShouldNot(HaveOccurred())

	return ginkgomon.New(ginkgomon.Config{
		Name:              "gnatsd",
		AnsiColorCode:     "30m",
		StartCheck:        "gnatsd is ready",
		StartCheckTimeout: 5 * time.Second,
		Command: exec.Command(
			"gnatsd",
			append([]string{
				"--addr", host,
				"--port", port,
			}, argv...)...,
		),
	})
}

func (maker ComponentMaker) Etcd(argv ...string) ifrit.Runner {
	nodeName := fmt.Sprintf("etcd_%d", ginkgo.GinkgoParallelNode())
	dataDir := path.Join(os.TempDir(), nodeName)

	return ginkgomon.New(ginkgomon.Config{
		Name:              "etcd",
		AnsiColorCode:     "31m",
		StartCheck:        "leader changed",
		StartCheckTimeout: 5 * time.Second,
		Command: exec.Command(
			"etcd",
			append([]string{
				"-data-dir", dataDir,
				"-addr", maker.Addresses.Etcd,
				"-peer-addr", maker.Addresses.EtcdPeer,
				"-name", nodeName,
			}, argv...)...,
		),
		Cleanup: func() {
			err := os.RemoveAll(dataDir)
			Ω(err).ShouldNot(HaveOccurred())
		},
	})
}

func (maker ComponentMaker) GardenLinux(argv ...string) *gardenrunner.Runner {
	return gardenrunner.New(
		"tcp",
		maker.Addresses.GardenLinux,
		maker.Artifacts.Executables["garden-linux"],
		maker.GardenBinPath,
		maker.GardenRootFSPath,
		maker.GardenGraphPath,
		argv...,
	)
}

func (maker ComponentMaker) Executor(argv ...string) *ginkgomon.Runner {
	tmpPath := path.Join(os.TempDir(), fmt.Sprintf("executor_%d", ginkgo.GinkgoParallelNode()))
	cachePath := path.Join(tmpPath, "cache")

	return ginkgomon.New(ginkgomon.Config{
		Name:          "executor",
		AnsiColorCode: "91m",
		StartCheck:    "executor.started",
		// executor may destroy containers on start, which can take a bit
		StartCheckTimeout: 30 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["exec"],
			append([]string{
				"-listenAddr", maker.Addresses.Executor,
				"-gardenNetwork", "tcp",
				"-gardenAddr", maker.Addresses.GardenLinux,
				"-loggregatorServer", maker.Addresses.LoggregatorIn,
				"-loggregatorSecret", "loggregator-secret",
				"-containerMaxCpuShares", "1024",
				"-cachePath", cachePath,
				"-tempDir", tmpPath,
			}, argv...)...,
		),
	})
}

func (maker ComponentMaker) Rep(argv ...string) ifrit.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:          "rep",
		AnsiColorCode: "92m",
		StartCheck:    "rep.started",
		// rep is not started until it can ping an executor; executor can take a
		// bit to start, so account for it
		StartCheckTimeout: 30 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["rep"],
			append(
				[]string{
					"-stack", maker.Stack,
					"-lrpHost", maker.ExternalAddress,
					"-etcdCluster", "http://" + maker.Addresses.Etcd,
					"-natsAddresses", maker.Addresses.NATS,
					"-executorID", "the-executor-id-" + strconv.Itoa(ginkgo.GinkgoParallelNode()),
					"-executorURL", "http://" + maker.Addresses.Executor,
					"-heartbeatInterval", "1s",
					"-pollingInterval", "1s",
				},
				argv...,
			)...,
		),
	})
}

func (maker ComponentMaker) Converger(argv ...string) ifrit.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:              "converger",
		AnsiColorCode:     "93m",
		StartCheck:        "converger.started",
		StartCheckTimeout: 5 * time.Second,

		Command: exec.Command(
			maker.Artifacts.Executables["converger"],
			append([]string{
				"-etcdCluster", "http://" + maker.Addresses.Etcd,
				"-heartbeatInterval", "1s",
			}, argv...)...,
		),
	})
}

func (maker ComponentMaker) Auctioneer(argv ...string) ifrit.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:              "auctioneer",
		AnsiColorCode:     "94m",
		StartCheck:        "auctioneer.started",
		StartCheckTimeout: 5 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["auctioneer"],
			append([]string{
				"-etcdCluster", "http://" + maker.Addresses.Etcd,
				"-natsAddresses", maker.Addresses.NATS,

				// inigo runs everything on the same machine, so there will be more
				// load; this timeout is a bit sensitive to that.
				"-natsAuctionTimeout", "5s",

				// we limit this to prevent overwhelming numbers of auctioneer logs.  it
				// should not impact the behavior of the tests.
				"-maxRounds", "3",
			}, argv...)...,
		),
	})
}

func (maker ComponentMaker) RouteEmitter(argv ...string) ifrit.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:              "route-emitter",
		AnsiColorCode:     "95m",
		StartCheck:        "route-emitter.started",
		StartCheckTimeout: 5 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["route-emitter"],
			append([]string{
				"-etcdCluster", "http://" + maker.Addresses.Etcd,
				"-natsAddresses", maker.Addresses.NATS,
			}, argv...)...,
		),
	})
}

func (maker ComponentMaker) TPS(argv ...string) ifrit.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:              "tps",
		AnsiColorCode:     "96m",
		StartCheck:        "tps.started",
		StartCheckTimeout: 5 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["tps"],
			append([]string{
				"-etcdCluster", "http://" + maker.Addresses.Etcd,
				"-natsAddresses", maker.Addresses.NATS,
				"-listenAddr", maker.Addresses.TPS,
			}, argv...)...,
		),
	})
}

func (maker ComponentMaker) NsyncListener(argv ...string) ifrit.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:              "nsync-listener",
		AnsiColorCode:     "97m",
		StartCheck:        "nsync.listener.started",
		StartCheckTimeout: 5 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["nsync-listener"],
			append([]string{
				"-etcdCluster", "http://" + maker.Addresses.Etcd,
				"-natsAddresses", maker.Addresses.NATS,
				"-circuses", fmt.Sprintf(`{"%s": "%s"}`, maker.Stack, CircusFilename),
				"-dockerCircusPath", DockerCircusFilename,
				"-fileServerURL", "http://" + maker.Addresses.FileServer,
			}, argv...)...,
		),
	})
}

func (maker ComponentMaker) FileServer(argv ...string) (ifrit.Runner, string) {
	servedFilesDir, err := ioutil.TempDir("", "file-server-files")
	Ω(err).ShouldNot(HaveOccurred())

	host, port, err := net.SplitHostPort(maker.Addresses.FileServer)
	Ω(err).ShouldNot(HaveOccurred())

	return ginkgomon.New(ginkgomon.Config{
		Name:              "file-server",
		AnsiColorCode:     "90m",
		StartCheck:        "file-server.ready",
		StartCheckTimeout: 5 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["file-server"],
			append([]string{
				"-address", host,
				"-port", port,
				"-ccAddress", "http://" + maker.Addresses.FakeCC,
				"-ccJobPollingInterval", "100ms",
				"-ccUsername", fake_cc.CC_USERNAME,
				"-ccPassword", fake_cc.CC_PASSWORD,
				"-staticDirectory", servedFilesDir,
			}, argv...)...,
		),
		Cleanup: func() {
			err := os.RemoveAll(servedFilesDir)
			Ω(err).ShouldNot(HaveOccurred())
		},
	}), servedFilesDir
}

func (maker ComponentMaker) Router() ifrit.Runner {
	_, routerPort, err := net.SplitHostPort(maker.Addresses.Router)
	Ω(err).ShouldNot(HaveOccurred())

	routerPortInt, err := strconv.Atoi(routerPort)
	Ω(err).ShouldNot(HaveOccurred())

	natsHost, natsPort, err := net.SplitHostPort(maker.Addresses.NATS)
	Ω(err).ShouldNot(HaveOccurred())

	natsPortInt, err := strconv.Atoi(natsPort)
	Ω(err).ShouldNot(HaveOccurred())

	routerConfig := &gorouterconfig.Config{
		Port: uint16(routerPortInt),

		PruneStaleDropletsIntervalInSeconds: 5,
		DropletStaleThresholdInSeconds:      10,
		PublishActiveAppsIntervalInSeconds:  0,
		StartResponseDelayIntervalInSeconds: 1,

		Nats: []gorouterconfig.NatsConfig{
			{
				Host: natsHost,
				Port: uint16(natsPortInt),
			},
		},
		Logging: gorouterconfig.LoggingConfig{
			File:  "/dev/stdout",
			Level: "info",
		},
	}

	configFile, err := ioutil.TempFile(os.TempDir(), "router-config")
	Ω(err).ShouldNot(HaveOccurred())

	defer configFile.Close()

	err = candiedyaml.NewEncoder(configFile).Encode(routerConfig)
	Ω(err).ShouldNot(HaveOccurred())

	return ginkgomon.New(ginkgomon.Config{
		Name:              "router",
		AnsiColorCode:     "32m",
		StartCheck:        "router.started",
		StartCheckTimeout: 5 * time.Second, // it waits 1 second before listening. yep.
		Command: exec.Command(
			maker.Artifacts.Executables["router"],
			"-c", configFile.Name(),
		),
		Cleanup: func() {
			err := os.Remove(configFile.Name())
			Ω(err).ShouldNot(HaveOccurred())
		},
	})
}

func (maker ComponentMaker) Loggregator() ifrit.Runner {
	_, inPort, err := net.SplitHostPort(maker.Addresses.LoggregatorIn)
	Ω(err).ShouldNot(HaveOccurred())

	_, outPort, err := net.SplitHostPort(maker.Addresses.LoggregatorOut)
	Ω(err).ShouldNot(HaveOccurred())

	inPortInt, err := strconv.Atoi(inPort)
	Ω(err).ShouldNot(HaveOccurred())

	outPortInt, err := strconv.Atoi(outPort)
	Ω(err).ShouldNot(HaveOccurred())

	natsHost, natsPort, err := net.SplitHostPort(maker.Addresses.NATS)
	Ω(err).ShouldNot(HaveOccurred())

	natsPortInt, err := strconv.Atoi(natsPort)
	Ω(err).ShouldNot(HaveOccurred())

	loggregatorConfig := LoggregatorConfig{
		LegacyIncomingMessagesPort: inPortInt,
		OutgoingPort:               outPortInt,
		MaxRetainedLogMessages:     1000,
		WSMessageBufferSize:        100,
		SharedSecret:               "loggregator-secret",
		NatsHost:                   natsHost,
		NatsPort:                   natsPortInt,
		InactivityDurationInMilliseconds: int((1 * time.Hour).Seconds()) * 1000,
	}

	configFile, err := ioutil.TempFile(os.TempDir(), "loggregator-config")
	Ω(err).ShouldNot(HaveOccurred())

	defer configFile.Close()

	err = json.NewEncoder(configFile).Encode(loggregatorConfig)
	Ω(err).ShouldNot(HaveOccurred())

	return ginkgomon.New(ginkgomon.Config{
		Name:              "loggregator",
		AnsiColorCode:     "33m",
		StartCheck:        "Listening on port",
		StartCheckTimeout: 5 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["loggregator"],
			"-config", configFile.Name(),
		),
		Cleanup: func() {
			err := os.Remove(configFile.Name())
			Ω(err).ShouldNot(HaveOccurred())
		},
	})
}

func (maker ComponentMaker) FakeCC() *fake_cc.FakeCC {
	return fake_cc.New(maker.Addresses.FakeCC)
}

func (maker ComponentMaker) Stager(argv ...string) ifrit.Runner {
	return maker.StagerN(0, argv...)
}

func (maker ComponentMaker) StagerN(portOffset int, argv ...string) ifrit.Runner {
	address := maker.Addresses.Stager
	port, err := strconv.Atoi(strings.Split(address, ":")[1])
	Ω(err).ShouldNot(HaveOccurred())

	return ginkgomon.New(ginkgomon.Config{
		Name:              "stager",
		AnsiColorCode:     "94m",
		StartCheck:        "Listening for staging requests!",
		StartCheckTimeout: 5 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["stager"],
			append([]string{
				"-natsAddresses", maker.Addresses.NATS,
				"-ccBaseURL", "http://" + maker.Addresses.FakeCC,
				"-ccUsername", fake_cc.CC_USERNAME,
				"-ccPassword", fake_cc.CC_PASSWORD,
				"-circuses", fmt.Sprintf(`{"%s": "%s"}`, maker.Stack, CircusFilename),
				"-diegoAPIURL", maker.Addresses.Receptor,
				"-stagerURL", fmt.Sprintf("http://127.0.0.1:%d", offsetPort(port, portOffset)),
				"-fileServerURL", "http://" + maker.Addresses.FileServer,
			}, argv...)...,
		),
	})
}

func (maker ComponentMaker) Receptor(argv ...string) ifrit.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:              "receptor",
		AnsiColorCode:     "37m",
		StartCheck:        "started",
		StartCheckTimeout: 5 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["receptor"],
			append([]string{
				"-address", maker.Addresses.Receptor,
				"-etcdCluster", "http://" + maker.Addresses.Etcd,
			}, argv...)...,
		),
	})
}

// offsetPort retuns a new port offest by a given number in such a way
// that it does not interfere with the ginkgo parallel node offest in the base port.
func offsetPort(basePort, offset int) int {
	return basePort + (10 * offset)
}
