package cell_test

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"

	archive_helper "code.cloudfoundry.org/archiver/extractor/test_helper"
	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/cfhttp"
	"code.cloudfoundry.org/inigo/fixtures"
	"code.cloudfoundry.org/inigo/helpers"
	"code.cloudfoundry.org/rep/cmd/rep/config"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
	"github.com/tedsuo/ifrit/grouper"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/ghttp"
)

var _ = Describe("Secure Downloading and Uploading", func() {
	var (
		processGuid         string
		archiveFiles        []archive_helper.ArchiveFile
		fileServerStaticDir string
		runtime             ifrit.Process
		tlsFileServer       *httptest.Server
	)

	BeforeEach(func() {
		processGuid = helpers.GenerateGuid()

		var fileServer ifrit.Runner
		fileServer, fileServerStaticDir = componentMaker.FileServer()
		runtime = ginkgomon.Invoke(grouper.NewParallel(os.Kill, grouper.Members{
			{Name: "file-server", Runner: fileServer},
			{Name: "rep", Runner: componentMaker.Rep(func(cfg *config.RepConfig) {
				cfg.PathToAssetTLSCert = "../fixtures/certs/client.crt"
				cfg.PathToAssetTLSKey = "../fixtures/certs/client.key"
				cfg.PathToAssetTLSCACert = "../fixtures/certs/ca.crt"
			})},
			{Name: "auctioneer", Runner: componentMaker.Auctioneer()},
		}))

		archiveFiles = fixtures.GoServerApp()
	})

	JustBeforeEach(func() {
		archive_helper.CreateZipArchive(
			filepath.Join(fileServerStaticDir, "lrp.zip"),
			archiveFiles,
		)
	})

	AfterEach(func() {
		helpers.StopProcesses(runtime)
	})

	Describe("downloading", func() {
		var lrp *models.DesiredLRP

		BeforeEach(func() {
			fileServerURL, err := url.Parse(fmt.Sprintf("http://%s", componentMaker.Addresses.FileServer))
			Expect(err).NotTo(HaveOccurred())
			proxy := httputil.NewSingleHostReverseProxy(fileServerURL)
			tlsFileServer = httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				proxy.ServeHTTP(rw, req)
			}))
			tlsConfig, err := cfhttp.NewTLSConfig(
				"../fixtures/certs/bbs_server.crt",
				"../fixtures/certs/bbs_server.key",
				"../fixtures/certs/ca.crt",
			)
			tlsFileServer.TLS = tlsConfig
			tlsFileServer.StartTLS()

			lrp = helpers.DefaultLRPCreateRequest(processGuid, "log-guid", 1)
			lrp.Setup = models.WrapAction(&models.DownloadAction{
				From: fmt.Sprintf("%s/v1/static/%s", tlsFileServer.URL, "lrp.zip"),
				To:   "/tmp",
				User: "vcap",
			})
		})

		JustBeforeEach(func() {
			err := bbsClient.DesireLRP(logger, lrp)
			Expect(err).NotTo(HaveOccurred())
		})

		It("eventually runs", func() {
			Eventually(helpers.LRPStatePoller(logger, bbsClient, processGuid, nil)).Should(Equal(models.ActualLRPStateRunning))
		})
	})

	Describe("uploading", func() {
		var (
			guid       string
			server     *httptest.Server
			uploadAddr string
			gotRequest chan struct{}
		)

		BeforeEach(func() {
			guid = helpers.GenerateGuid()

			gotRequest = make(chan struct{})

			server, uploadAddr = helpers.Callback(componentMaker.ExternalAddress, ghttp.CombineHandlers(
				ghttp.VerifyRequest("POST", "/thingy"),
				func(w http.ResponseWriter, r *http.Request) {
					contents, err := ioutil.ReadAll(r.Body)
					Expect(err).NotTo(HaveOccurred())

					Expect(string(contents)).To(Equal("tasty thingy\n"))

					close(gotRequest)
				},
			))

			serverURL, err := url.Parse(server.URL)
			Expect(err).NotTo(HaveOccurred())
			proxy := httputil.NewSingleHostReverseProxy(serverURL)
			tlsFileServer = httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				proxy.ServeHTTP(rw, req)
			}))
			tlsConfig, err := cfhttp.NewTLSConfig(
				"../fixtures/certs/bbs_server.crt",
				"../fixtures/certs/bbs_server.key",
				"../fixtures/certs/ca.crt",
			)
			tlsFileServer.TLS = tlsConfig
			tlsFileServer.StartTLS()
		})

		AfterEach(func() {
			server.Close()
		})

		It("uploads the specified files", func() {
			expectedTask := helpers.TaskCreateRequest(
				guid,
				models.Serial(
					&models.RunAction{
						User: "vcap",
						Path: "sh",
						Args: []string{"-c", "echo tasty thingy > thingy"},
					},
					&models.UploadAction{
						From: "thingy",
						To:   fmt.Sprintf("%s/thingy", tlsFileServer.URL),
						User: "vcap",
					},
				),
			)

			err := bbsClient.DesireTask(logger, expectedTask.TaskGuid, expectedTask.Domain, expectedTask.TaskDefinition)
			Expect(err).NotTo(HaveOccurred())

			Eventually(gotRequest).Should(BeClosed())
		})
	})
})
