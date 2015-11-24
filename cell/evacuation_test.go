package cell_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/cloudfoundry-incubator/inigo/fixtures"
	"github.com/cloudfoundry-incubator/inigo/helpers"
	"github.com/pivotal-golang/archiver/extractor/test_helper"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
	"github.com/tedsuo/ifrit/grouper"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Evacuation", func() {
	var (
		runtime ifrit.Process

		cellAID           string
		cellAExecutorAddr string
		cellARepAddr      string

		cellBID           string
		cellBExecutorAddr string
		cellBRepAddr      string

		cellARepRunner *ginkgomon.Runner
		cellBRepRunner *ginkgomon.Runner

		cellA ifrit.Process
		cellB ifrit.Process

		processGuid string
		appId       string
	)

	BeforeEach(func() {
		processGuid = helpers.GenerateGuid()
		appId = helpers.GenerateGuid()

		fileServer, fileServerStaticDir := componentMaker.FileServer()

		runtime = ginkgomon.Invoke(grouper.NewParallel(os.Kill, grouper.Members{
			{"router", componentMaker.Router()},
			{"file-server", fileServer},
			{"converger", componentMaker.Converger("-convergeRepeatInterval", "1s")},
			{"auctioneer", componentMaker.Auctioneer()},
			{"route-emitter", componentMaker.RouteEmitter()},
		}))

		cellAID = "cell-a"
		cellBID = "cell-b"

		cellAExecutorAddr = fmt.Sprintf("127.0.0.1:%d", 13100+GinkgoParallelNode())
		cellBExecutorAddr = fmt.Sprintf("127.0.0.1:%d", 13200+GinkgoParallelNode())

		cellARepAddr = fmt.Sprintf("0.0.0.0:%d", 14100+GinkgoParallelNode())
		cellBRepAddr = fmt.Sprintf("0.0.0.0:%d", 14200+GinkgoParallelNode())

		cellARepRunner = componentMaker.RepN(0,
			"-cellID", cellAID,
			"-listenAddr", cellARepAddr,
			"-evacuationTimeout", "30s",
			"-containerOwnerName", cellAID+"-executor",
		)

		cellBRepRunner = componentMaker.RepN(1,
			"-cellID", cellBID,
			"-listenAddr", cellBRepAddr,
			"-evacuationTimeout", "30s",
			"-containerOwnerName", cellBID+"-executor",
		)

		cellA = ginkgomon.Invoke(cellARepRunner)
		cellB = ginkgomon.Invoke(cellBRepRunner)

		test_helper.CreateZipArchive(
			filepath.Join(fileServerStaticDir, "lrp.zip"),
			fixtures.GoServerApp(),
		)
	})

	AfterEach(func() {
		helpers.StopProcesses(runtime, cellA, cellB)
	})

	It("handles evacuation", func() {
		By("desiring an LRP")
		lrp := helpers.DefaultLRPCreateRequest(processGuid, "log-guid", 1)
		lrp.Setup = models.WrapAction(&models.DownloadAction{
			From: fmt.Sprintf("http://%s/v1/static/%s", componentMaker.Addresses.FileServer, "lrp.zip"),
			To:   "/tmp",
			User: "vcap",
		})
		lrp.Action = models.WrapAction(&models.RunAction{
			User: "vcap",
			Path: "/tmp/go-server",
			Env:  []*models.EnvironmentVariable{{"PORT", "8080"}},
		})

		err := bbsClient.DesireLRP(lrp)
		Expect(err).NotTo(HaveOccurred())

		By("running an actual LRP instance")
		Eventually(helpers.LRPStatePoller(bbsClient, processGuid, nil)).Should(Equal(models.ActualLRPStateRunning))
		Eventually(helpers.ResponseCodeFromHostPoller(componentMaker.Addresses.Router, helpers.DefaultHost)).Should(Equal(http.StatusOK))

		actualLRPGroup, err := bbsClient.ActualLRPGroupByProcessGuidAndIndex(processGuid, 0)
		Expect(err).NotTo(HaveOccurred())

		actualLRP, isEvacuating := actualLRPGroup.Resolve()
		Expect(isEvacuating).To(BeFalse())

		var evacuatingRepAddr string
		var evacutaingRepRunner *ginkgomon.Runner

		switch actualLRP.CellId {
		case cellAID:
			evacuatingRepAddr = cellARepAddr
			evacutaingRepRunner = cellARepRunner
		case cellBID:
			evacuatingRepAddr = cellBRepAddr
			evacutaingRepRunner = cellBRepRunner
		default:
			panic("what? who?")
		}

		By("posting the evacuation endpoint")
		resp, err := http.Post(fmt.Sprintf("http://%s/evacuate", evacuatingRepAddr), "text/html", nil)
		Expect(err).NotTo(HaveOccurred())
		resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusAccepted))

		By("staying routable so long as its rep is alive")
		Eventually(func() int {
			Expect(helpers.ResponseCodeFromHostPoller(componentMaker.Addresses.Router, helpers.DefaultHost)()).To(Equal(http.StatusOK))
			return evacutaingRepRunner.ExitCode()
		}).Should(Equal(0))

		By("running immediately after the rep exits and is eventually routable")
		Expect(helpers.LRPStatePoller(bbsClient, processGuid, nil)()).To(Equal(models.ActualLRPStateRunning))
		Eventually(helpers.ResponseCodeFromHostPoller(componentMaker.Addresses.Router, helpers.DefaultHost)).Should(Equal(http.StatusOK))
		Consistently(helpers.ResponseCodeFromHostPoller(componentMaker.Addresses.Router, helpers.DefaultHost)).Should(Equal(http.StatusOK))
	})
})
