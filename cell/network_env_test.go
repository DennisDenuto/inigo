package cell_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/cloudfoundry-incubator/inigo/fixtures"
	"github.com/cloudfoundry-incubator/inigo/helpers"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pivotal-golang/archiver/extractor/test_helper"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
	"github.com/tedsuo/ifrit/grouper"
)

var _ = Describe("Network Environment Variables", func() {
	var (
		guid                string
		repFlags            []string
		fileServerStaticDir string
		fileServer          ifrit.Runner
		runtime             ifrit.Process
	)

	BeforeEach(func() {
		fileServer, fileServerStaticDir = componentMaker.FileServer()

		repFlags = []string{}
		guid = helpers.GenerateGuid()
	})

	JustBeforeEach(func() {
		runtime = ginkgomon.Invoke(grouper.NewParallel(os.Kill, grouper.Members{
			{"rep", componentMaker.Rep(repFlags...)},
			{"converger", componentMaker.Converger()},
			{"auctioneer", componentMaker.Auctioneer()},
			{"router", componentMaker.Router()},
			{"route-emitter", componentMaker.RouteEmitter()},
			{"file-server", fileServer},
		}))
	})

	AfterEach(func() {
		helpers.StopProcesses(runtime)
	})

	Describe("tasks", func() {
		var task *models.Task

		JustBeforeEach(func() {
			taskToDesire := helpers.TaskCreateRequest(
				guid,
				&models.RunAction{
					User: "vcap",
					Path: "sh",
					Args: []string{"-c", "/usr/bin/env | grep 'CF_INSTANCE' > /home/vcap/env"},
				},
			)
			taskToDesire.ResultFile = "/home/vcap/env"

			err := bbsClient.DesireTask(taskToDesire.TaskGuid, taskToDesire.Domain, taskToDesire.TaskDefinition)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() interface{} {
				var err error
				task, err = bbsClient.TaskByGuid(guid)
				Expect(err).ShouldNot(HaveOccurred())

				return task.State
			}).Should(Equal(models.Task_Completed))
		})

		Context("when -exportNetworkEnvVars=false is set", func() {
			BeforeEach(func() {
				repFlags = []string{"-exportNetworkEnvVars=false"}
			})

			It("does not set the networking environment variables", func() {
				Expect(task.Result).To(Equal(""))
			})
		})

		Context("when -exportNetworkEnvVars=true is set", func() {
			BeforeEach(func() {
				repFlags = []string{"-exportNetworkEnvVars=true"}
			})

			It("sets the networking environment variables", func() {
				Expect(task.Result).To(ContainSubstring("CF_INSTANCE_ADDR=\n"))
				Expect(task.Result).To(ContainSubstring("CF_INSTANCE_PORT=\n"))
				Expect(task.Result).To(ContainSubstring("CF_INSTANCE_PORTS=[]\n"))
				Expect(task.Result).To(ContainSubstring(fmt.Sprintf("CF_INSTANCE_IP=%s\n", componentMaker.ExternalAddress)))
			})
		})
	})

	Describe("LRPs", func() {
		var response []byte

		BeforeEach(func() {
			test_helper.CreateZipArchive(
				filepath.Join(fileServerStaticDir, "lrp.zip"),
				fixtures.GoServerApp(),
			)
		})

		JustBeforeEach(func() {
			lrp := helpers.DefaultLRPCreateRequest(guid, guid, 1)
			lrp.Setup = models.WrapAction(&models.DownloadAction{
				User: "vcap",
				From: fmt.Sprintf("http://%s/v1/static/%s", componentMaker.Addresses.FileServer, "lrp.zip"),
				To:   "/tmp",
			})
			lrp.Action = models.WrapAction(&models.RunAction{
				User: "vcap",
				Path: "/tmp/go-server",
				Env:  []*models.EnvironmentVariable{{"PORT", "8080"}},
			})

			err := bbsClient.DesireLRP(lrp)
			Expect(err).NotTo(HaveOccurred())

			Eventually(helpers.LRPStatePoller(bbsClient, guid, nil)).Should(Equal(models.ActualLRPStateRunning))
			Eventually(helpers.ResponseCodeFromHostPoller(componentMaker.Addresses.Router, helpers.DefaultHost)).Should(Equal(http.StatusOK))

			response, _, err = helpers.ResponseBodyAndStatusCodeFromHost(componentMaker.Addresses.Router, helpers.DefaultHost, "env")
			Expect(err).NotTo(HaveOccurred())
		})

		Context("when -exportNetworkEnvVars=false is set", func() {
			BeforeEach(func() {
				repFlags = []string{"-exportNetworkEnvVars=false"}
			})

			It("does not set the networking environment variables", func() {
				Expect(response).NotTo(ContainSubstring("CF_INSTANCE_ADDR="))
				Expect(response).NotTo(ContainSubstring("CF_INSTANCE_PORT="))
				Expect(response).NotTo(ContainSubstring("CF_INSTANCE_PORTS=[]"))
				Expect(response).NotTo(ContainSubstring(fmt.Sprintf("CF_INSTANCE_IP=%s", componentMaker.ExternalAddress)))
			})
		})

		Context("when -exportNetworkEnvVars=true is set", func() {
			BeforeEach(func() {
				repFlags = []string{"-exportNetworkEnvVars=true"}
			})

			It("sets the networking environment variables", func() {
				Expect(response).To(ContainSubstring("CF_INSTANCE_ADDR=\n"))
				Expect(response).To(ContainSubstring("CF_INSTANCE_PORT=\n"))
				Expect(response).To(ContainSubstring("CF_INSTANCE_PORTS=[]\n"))
				Expect(response).To(ContainSubstring(fmt.Sprintf("CF_INSTANCE_IP=%s\n", componentMaker.ExternalAddress)))
			})
		})
	})
})
