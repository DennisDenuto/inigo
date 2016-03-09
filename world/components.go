package world

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cloudfoundry-incubator/bbs"
	"github.com/cloudfoundry-incubator/candiedyaml"
	"github.com/cloudfoundry-incubator/consuladapter"
	"github.com/cloudfoundry-incubator/consuladapter/consulrunner"
	"github.com/cloudfoundry-incubator/garden"
	gardenclient "github.com/cloudfoundry-incubator/garden/client"
	gardenconnection "github.com/cloudfoundry-incubator/garden/client/connection"
	"github.com/cloudfoundry-incubator/inigo/fake_cc"
	"github.com/cloudfoundry-incubator/inigo/gardenrunner"
	"github.com/cloudfoundry-incubator/volman"
	volmanclient "github.com/cloudfoundry-incubator/volman/vollocal"
	gorouterconfig "github.com/cloudfoundry/gorouter/config"
	"github.com/cloudfoundry/gunk/diegonats"
	"github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pivotal-golang/clock"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
	"golang.org/x/crypto/ssh"
	"github.com/cloudfoundry-incubator/volman/voldriver"
	"path/filepath"
)

type BuiltExecutables map[string]string
type BuiltLifecycles map[string]string

const LifecycleFilename = "some-lifecycle.tar.gz"

type BuiltArtifacts struct {
	Executables BuiltExecutables
	Lifecycles  BuiltLifecycles
}

type SSHKeys struct {
	HostKey       ssh.Signer
	HostKeyPem    string
	PrivateKeyPem string
	AuthorizedKey string
}

type SSLConfig struct {
	ServerCert string
	ServerKey  string
	ClientCert string
	ClientKey  string
	CACert     string
}

type ComponentAddresses struct {
	NATS          string
	Etcd          string
	EtcdPeer      string
	Consul        string
	BBS           string
	Rep           string
	FakeCC        string
	FileServer    string
	CCUploader    string
	Router        string
	TPSListener   string
	GardenLinux   string
	Stager        string
	NsyncListener string
	Auctioneer    string
	SSHProxy      string
}

type ComponentMaker struct {
	Artifacts BuiltArtifacts
	Addresses ComponentAddresses

	ExternalAddress string

	PreloadedStackPathMap map[string]string

	GardenBinPath   string
	GardenGraphPath string

	SSHConfig SSHKeys
	EtcdSSL   SSLConfig
	BbsSSL    SSLConfig
}

func (maker ComponentMaker) NATS(argv ...string) ifrit.Runner {
	host, port, err := net.SplitHostPort(maker.Addresses.NATS)
	Expect(err).NotTo(HaveOccurred())

	return ginkgomon.New(ginkgomon.Config{
		Name:              "gnatsd",
		AnsiColorCode:     "30m",
		StartCheck:        "gnatsd is ready",
		StartCheckTimeout: 10 * time.Second,
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
		StartCheck:        "etcdserver: published",
		StartCheckTimeout: 10 * time.Second,
		Command: exec.Command(
			"etcd",
			append([]string{
				"--name", nodeName,
				"--data-dir", dataDir,
				"--listen-client-urls", "https://" + maker.Addresses.Etcd,
				"--listen-peer-urls", "http://" + maker.Addresses.EtcdPeer,
				"--initial-cluster", nodeName + "=" + "http://" + maker.Addresses.EtcdPeer,
				"--initial-advertise-peer-urls", "http://" + maker.Addresses.EtcdPeer,
				"--initial-cluster-state", "new",
				"--advertise-client-urls", "https://" + maker.Addresses.Etcd,
				"--cert-file", maker.EtcdSSL.ServerCert,
				"--key-file", maker.EtcdSSL.ServerKey,
				"--ca-file", maker.EtcdSSL.CACert,
			}, argv...)...,
		),
		Cleanup: func() {
			err := os.RemoveAll(dataDir)
			Expect(err).NotTo(HaveOccurred())
		},
	})
}

func (maker ComponentMaker) Consul(argv ...string) ifrit.Runner {
	_, port, err := net.SplitHostPort(maker.Addresses.Consul)
	Expect(err).NotTo(HaveOccurred())
	httpPort, err := strconv.Atoi(port)
	Expect(err).NotTo(HaveOccurred())

	startingPort := httpPort - consulrunner.PortOffsetHTTP

	clusterRunner := consulrunner.NewClusterRunner(startingPort, 1, "http")
	return ifrit.RunFunc(func(signals <-chan os.Signal, ready chan<- struct{}) error {
		done := make(chan struct{})
		go func() {
			clusterRunner.Start()
			close(done)
		}()

		Eventually(done, 10).Should(BeClosed())

		close(ready)

		select {
		case <-signals:
			clusterRunner.Stop()
		}

		return nil
	})
}

func (maker ComponentMaker) GardenLinux(argv ...string) *gardenrunner.Runner {
	return gardenrunner.New(
		"tcp",
		maker.Addresses.GardenLinux,
		maker.Artifacts.Executables["garden-linux"],
		maker.GardenBinPath,
		maker.PreloadedStackPathMap[maker.DefaultStack()],
		maker.GardenGraphPath,
		argv...,
	)
}

func (maker ComponentMaker) BBS(argv ...string) ifrit.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:              "bbs",
		AnsiColorCode:     "32m",
		StartCheck:        "bbs.started",
		StartCheckTimeout: 10 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["bbs"],
			append([]string{
				"-activeKeyLabel=" + "secure-key-1",
				"-advertiseURL", maker.BBSURL(),
				"-auctioneerAddress", "http://" + maker.Addresses.Auctioneer,
				"-consulCluster", maker.ConsulCluster(),
				"-encryptionKey=" + "secure-key-1:secure-passphrase",
				"-etcdCaFile", maker.EtcdSSL.CACert,
				"-etcdCertFile", maker.EtcdSSL.ClientCert,
				"-etcdCluster", maker.EtcdCluster(),
				"-etcdKeyFile", maker.EtcdSSL.ClientKey,
				"-listenAddress", maker.Addresses.BBS,
				"-logLevel", "debug",
				"-requireSSL",
				"-certFile", maker.BbsSSL.ServerCert,
				"-keyFile", maker.BbsSSL.ServerKey,
				"-caFile", maker.BbsSSL.CACert,
			}, argv...)...,
		),
	})
}

func (maker ComponentMaker) Rep(argv ...string) *ginkgomon.Runner {
	return maker.RepN(0, argv...)
}

func (maker ComponentMaker) RepN(n int, argv ...string) *ginkgomon.Runner {
	host, portString, err := net.SplitHostPort(maker.Addresses.Rep)
	Expect(err).NotTo(HaveOccurred())
	port, err := strconv.Atoi(portString)
	Expect(err).NotTo(HaveOccurred())

	name := "rep-" + strconv.Itoa(n)

	tmpDir, err := ioutil.TempDir(os.TempDir(), "executor")
	Expect(err).NotTo(HaveOccurred())

	cachePath := path.Join(tmpDir, "cache")

	args := append(
		[]string{
			"-sessionName", name,
			"-rootFSProvider", "docker",
			"-bbsAddress", maker.BBSURL(),
			"-listenAddr", fmt.Sprintf("%s:%d", host, offsetPort(port, n)),
			"-cellID", "the-cell-id-" + strconv.Itoa(ginkgo.GinkgoParallelNode()) + "-" + strconv.Itoa(n),
			"-pollingInterval", "1s",
			"-evacuationPollingInterval", "1s",
			"-evacuationTimeout", "1s",
			"-lockTTL", "10s",
			"-lockRetryInterval", "1s",
			"-consulCluster", maker.ConsulCluster(),
			"-gardenNetwork", "tcp",
			"-gardenAddr", maker.Addresses.GardenLinux,
			"-containerMaxCpuShares", "1024",
			"-cachePath", cachePath,
			"-tempDir", tmpDir,
			"-logLevel", "debug",
			"-bbsClientCert", maker.BbsSSL.ClientCert,
			"-bbsClientKey", maker.BbsSSL.ClientKey,
			"-bbsCACert", maker.BbsSSL.CACert,
			"-gardenHealthcheckProcessPath", "/bin/sh",
			"-gardenHealthcheckProcessArgs", "-c,echo,foo",
			"-gardenHealthcheckProcessUser", "vcap",
		},
		argv...,
	)
	for stack, path := range maker.PreloadedStackPathMap {
		args = append(args, "-preloadedRootFS", fmt.Sprintf("%s:%s", stack, path))
	}

	return ginkgomon.New(ginkgomon.Config{
		Name:          name,
		AnsiColorCode: "33m",
		StartCheck:    `"` + name + `.started"`,
		// rep is not started until it can ping an executor and run a healthcheck
		// container on garden; this can take a bit to start, so account for it
		StartCheckTimeout: 2 * time.Minute,
		Command:           exec.Command(maker.Artifacts.Executables["rep"], args...),
		Cleanup: func() {
			os.RemoveAll(tmpDir)
		},
	})
}

func (maker ComponentMaker) Converger(argv ...string) ifrit.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:              "converger",
		AnsiColorCode:     "34m",
		StartCheck:        `"converger.started"`,
		StartCheckTimeout: 15 * time.Second,

		Command: exec.Command(
			maker.Artifacts.Executables["converger"],
			append([]string{
				"-bbsAddress", maker.BBSURL(),
				"-lockTTL", "10s",
				"-lockRetryInterval", "1s",
				"-consulCluster", maker.ConsulCluster(),
				"-logLevel", "debug",
				"-bbsClientCert", maker.BbsSSL.ClientCert,
				"-bbsClientKey", maker.BbsSSL.ClientKey,
				"-bbsCACert", maker.BbsSSL.CACert,
			}, argv...)...,
		),
	})
}

func (maker ComponentMaker) Auctioneer(argv ...string) ifrit.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:              "auctioneer",
		AnsiColorCode:     "35m",
		StartCheck:        `"auctioneer.started"`,
		StartCheckTimeout: 10 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["auctioneer"],
			append([]string{
				"-bbsAddress", maker.BBSURL(),
				"-listenAddr", maker.Addresses.Auctioneer,
				"-lockRetryInterval", "1s",
				"-consulCluster", maker.ConsulCluster(),
				"-logLevel", "debug",
				"-bbsClientCert", maker.BbsSSL.ClientCert,
				"-bbsClientKey", maker.BbsSSL.ClientKey,
				"-bbsCACert", maker.BbsSSL.CACert,
				"-startingContainerWeight", "0.33",
			}, argv...)...,
		),
	})
}

func (maker ComponentMaker) RouteEmitter(argv ...string) ifrit.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:              "route-emitter",
		AnsiColorCode:     "36m",
		StartCheck:        `"route-emitter.started"`,
		StartCheckTimeout: 10 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["route-emitter"],
			append([]string{
				"-natsAddresses", maker.Addresses.NATS,
				"-bbsAddress", maker.BBSURL(),
				"-lockRetryInterval", "1s",
				"-consulCluster", maker.ConsulCluster(),
				"-logLevel", "debug",
				"-bbsClientCert", maker.BbsSSL.ClientCert,
				"-bbsClientKey", maker.BbsSSL.ClientKey,
				"-bbsCACert", maker.BbsSSL.CACert,
			}, argv...)...,
		),
	})
}

func (maker ComponentMaker) TPSListener(argv ...string) ifrit.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:              "tps-listener",
		AnsiColorCode:     "37m",
		StartCheck:        `"tps-listener.started"`,
		StartCheckTimeout: 10 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["tps-listener"],
			append([]string{
				"-bbsAddress", maker.BBSURL(),
				"-bbsCACert", maker.BbsSSL.CACert,
				"-bbsClientCert", maker.BbsSSL.ClientCert,
				"-bbsClientKey", maker.BbsSSL.ClientKey,
				"-consulCluster", maker.ConsulCluster(),
				"-listenAddr", maker.Addresses.TPSListener,
				"-logLevel", "debug",
			}, argv...)...,
		),
	})
}

func (maker ComponentMaker) NsyncListener(argv ...string) ifrit.Runner {
	address := maker.Addresses.NsyncListener
	port, err := strconv.Atoi(strings.Split(address, ":")[1])
	Expect(err).NotTo(HaveOccurred())

	return ginkgomon.New(ginkgomon.Config{
		Name:              "nsync-listener",
		AnsiColorCode:     "90m",
		StartCheck:        `"nsync.listener.started"`,
		StartCheckTimeout: 10 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["nsync-listener"],
			append(maker.appendLifecycleArgs([]string{
				"-bbsAddress", maker.BBSURL(),
				"-bbsCACert", maker.BbsSSL.CACert,
				"-bbsClientCert", maker.BbsSSL.ClientCert,
				"-bbsClientKey", maker.BbsSSL.ClientKey,
				"-consulCluster", maker.ConsulCluster(),
				"-fileServerURL", "http://" + maker.Addresses.FileServer,
				"-listenAddress", fmt.Sprintf("127.0.0.1:%d", port),
				"-logLevel", "debug",
			}), argv...)...,
		),
	})
}

func (maker ComponentMaker) CCUploader(argv ...string) ifrit.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:              "cc-uploader",
		AnsiColorCode:     "91m",
		StartCheck:        `"cc-uploader.ready"`,
		StartCheckTimeout: 10 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["cc-uploader"],
			append([]string{
				"-address", maker.Addresses.CCUploader,
				"-ccJobPollingInterval", "100ms",
				"-consulCluster", maker.ConsulCluster(),
				"-logLevel", "debug",
			}, argv...)...,
		),
	})
}

func (maker ComponentMaker) FileServer(argv ...string) (ifrit.Runner, string) {
	servedFilesDir, err := ioutil.TempDir("", "file-server-files")
	Expect(err).NotTo(HaveOccurred())

	return ginkgomon.New(ginkgomon.Config{
		Name:              "file-server",
		AnsiColorCode:     "92m",
		StartCheck:        `"file-server.ready"`,
		StartCheckTimeout: 10 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["file-server"],
			append([]string{
				"-address", maker.Addresses.FileServer,
				"-consulCluster", maker.ConsulCluster(),
				"-logLevel", "debug",
				"-staticDirectory", servedFilesDir,
			}, argv...)...,
		),
		Cleanup: func() {
			err := os.RemoveAll(servedFilesDir)
			Expect(err).NotTo(HaveOccurred())
		},
	}), servedFilesDir
}

func (maker ComponentMaker) Router() ifrit.Runner {
	_, routerPort, err := net.SplitHostPort(maker.Addresses.Router)
	Expect(err).NotTo(HaveOccurred())

	routerPortInt, err := strconv.Atoi(routerPort)
	Expect(err).NotTo(HaveOccurred())

	natsHost, natsPort, err := net.SplitHostPort(maker.Addresses.NATS)
	Expect(err).NotTo(HaveOccurred())

	natsPortInt, err := strconv.Atoi(natsPort)
	Expect(err).NotTo(HaveOccurred())

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
			File:          "/dev/stdout",
			Level:         "info",
			MetronAddress: "127.0.0.1:65534", // nonsense to make dropsonde happy
		},
	}

	configFile, err := ioutil.TempFile(os.TempDir(), "router-config")
	Expect(err).NotTo(HaveOccurred())

	defer configFile.Close()

	err = candiedyaml.NewEncoder(configFile).Encode(routerConfig)
	Expect(err).NotTo(HaveOccurred())

	return ginkgomon.New(ginkgomon.Config{
		Name:              "router",
		AnsiColorCode:     "93m",
		StartCheck:        "router.started",
		StartCheckTimeout: 10 * time.Second, // it waits 1 second before listening. yep.
		Command: exec.Command(
			maker.Artifacts.Executables["router"],
			"-c", configFile.Name(),
		),
		Cleanup: func() {
			err := os.Remove(configFile.Name())
			Expect(err).NotTo(HaveOccurred())
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
	Expect(err).NotTo(HaveOccurred())

	return ginkgomon.New(ginkgomon.Config{
		Name:              "stager",
		AnsiColorCode:     "94m",
		StartCheck:        "Listening for staging requests!",
		StartCheckTimeout: 10 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["stager"],
			append(maker.appendLifecycleArgs([]string{
				"-bbsAddress", maker.BBSURL(),
				"-bbsCACert", maker.BbsSSL.CACert,
				"-bbsClientCert", maker.BbsSSL.ClientCert,
				"-bbsClientKey", maker.BbsSSL.ClientKey,
				"-ccBaseURL", "http://" + maker.Addresses.FakeCC,
				"-ccPassword", fake_cc.CC_PASSWORD,
				"-ccUploaderURL", "http://" + maker.Addresses.CCUploader,
				"-ccUsername", fake_cc.CC_USERNAME,
				"-consulCluster", maker.ConsulCluster(),
				"-dockerStagingStack", maker.DefaultStack(),
				"-fileServerURL", "http://" + maker.Addresses.FileServer,
				"-listenAddress", fmt.Sprintf("127.0.0.1:%d", offsetPort(port, portOffset)),
				"-logLevel", "debug",
				"-stagingTaskCallbackURL", fmt.Sprintf("http://127.0.0.1:%d", offsetPort(port, portOffset)),
			}), argv...)...,
		),
	})
}

func (maker ComponentMaker) SSHProxy(argv ...string) ifrit.Runner {
	return ginkgomon.New(ginkgomon.Config{
		Name:              "ssh-proxy",
		AnsiColorCode:     "96m",
		StartCheck:        "ssh-proxy.started",
		StartCheckTimeout: 10 * time.Second,
		Command: exec.Command(
			maker.Artifacts.Executables["ssh-proxy"],
			append([]string{
				"-address", maker.Addresses.SSHProxy,
				"-bbsAddress", maker.BBSURL(),
				"-bbsCACert", maker.BbsSSL.CACert,
				"-bbsClientCert", maker.BbsSSL.ClientCert,
				"-bbsClientKey", maker.BbsSSL.ClientKey,
				"-consulCluster", maker.ConsulCluster(),
				"-enableDiegoAuth",
				"-hostKey", maker.SSHConfig.HostKeyPem,
				"-logLevel", "debug",
			}, argv...)...,
		),
	})
}

func (maker ComponentMaker) appendLifecycleArgs(args []string) []string {
	for stack, _ := range maker.PreloadedStackPathMap {
		args = append(args, "-lifecycle", fmt.Sprintf("buildpack/%s:%s", stack, LifecycleFilename))
	}

	return args
}

func (maker ComponentMaker) DefaultStack() string {
	Expect(maker.PreloadedStackPathMap).NotTo(BeEmpty())

	var defaultStack string
	for stack, _ := range maker.PreloadedStackPathMap {
		defaultStack = stack
		break
	}

	return defaultStack
}

func (maker ComponentMaker) NATSClient() diegonats.NATSClient {
	client := diegonats.NewClient()

	_, err := client.Connect([]string{"nats://" + maker.Addresses.NATS})
	Expect(err).NotTo(HaveOccurred())

	return client
}

func (maker ComponentMaker) GardenClient() garden.Client {
	return gardenclient.New(gardenconnection.New("tcp", maker.Addresses.GardenLinux))
}

func (maker ComponentMaker) BBSClient() bbs.Client {
	client, err := bbs.NewSecureClient(
		maker.BBSURL(),
		maker.BbsSSL.CACert,
		maker.BbsSSL.ClientCert,
		maker.BbsSSL.ClientKey,
		0, 0,
	)
	Expect(err).NotTo(HaveOccurred())
	return client
}

func (maker ComponentMaker) BBSServiceClient(logger lager.Logger) bbs.ServiceClient {
	client, err := consuladapter.NewClientFromUrl(maker.ConsulCluster())
	Expect(err).NotTo(HaveOccurred())

	return bbs.NewServiceClient(client, clock.NewClock())
}

func (maker ComponentMaker) BBSURL() string {
	return "https://" + maker.Addresses.BBS
}

func (maker ComponentMaker) ConsulCluster() string {
	return "http://" + maker.Addresses.Consul
}

func (maker ComponentMaker) EtcdCluster() string {
	return "https://" + maker.Addresses.Etcd
}

func (maker ComponentMaker) VolmanClient(driverPath string) volman.Manager {
	return volmanclient.NewLocalClient(driverPath)
}

func (make ComponentMaker) VolmanDriver(logger lager.Logger, fakeDriverPath string) ifrit.Runner {
	fakedriverServerPort := 9750 + ginkgo.GinkgoParallelNode()
	debugServerAddress := fmt.Sprintf("0.0.0.0:%d", 9850+ginkgo.GinkgoParallelNode())
	url := fmt.Sprintf("0.0.0.0:%d", fakedriverServerPort)
	fakeDriverRunner := ginkgomon.New(ginkgomon.Config{
		Name: "fakedriverServer",
		Command: exec.Command(
			fakeDriverPath,
			"-listenAddr", url,
			"-debugAddr", debugServerAddress,
		),
		StartCheck: "fakedriverServer.started",
	})
	parentPath := filepath.Dir(strings.Split(fakeDriverPath, ",")[0])
	voldriver.WriteDriverSpec(logger, parentPath, "fakedriver", "http://" + url)
	return fakeDriverRunner
}

// offsetPort retuns a new port offest by a given number in such a way
// that it does not interfere with the ginkgo parallel node offest in the base port.
func offsetPort(basePort, offset int) int {
	return basePort + (10 * offset)
}
