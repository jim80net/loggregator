package integration_tests

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"

	dopplerConfig "doppler/config"
	metronConfig "metron/config"
	trafficcontrollerConfig "trafficcontroller/config"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/config"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
	"github.com/pivotal-golang/localip"
)

const (
	sharedSecret     = "test-shared-secret"
	availabilityZone = "test-availability-zone"
	jobName          = "test-job-name"
	jobIndex         = "42"

	portRangeStart       = 55000
	portRangeCoefficient = 100
	etcdPortOffset       = iota
	etcdPeerPortOffset
	dopplerUDPPortOffset
	dopplerTCPPortOffset
	dopplerTLSPortOffset
	dopplerOutgoingPortOffset
	metronPortOffset
	trafficcontrollerPortOffset
)

// TODO: Add color to writers
const (
	yellow = 33 + iota
	blue
	magenta
	cyan
	stdOut = "\x1b[32m[o]\x1b[%dm[%s]\x1b[0m "
	stdErr = "\x1b[31m[e]\x1b[%dm[%s]\x1b[0m "
)

func getPort(offset int) int {
	return config.GinkgoConfig.ParallelNode*portRangeCoefficient + portRangeStart + offset
}

func SetupEtcd() (func(), string) {
	By("making sure etcd was build")
	etcdPath := os.Getenv("ETCD_BUILD_PATH")
	Expect(etcdPath).ToNot(BeEmpty())

	By("starting etcd")
	etcdPort := getPort(etcdPortOffset)
	etcdPeerPort := getPort(etcdPeerPortOffset)
	etcdClientURL := fmt.Sprintf("http://localhost:%d", etcdPort)
	etcdPeerURL := fmt.Sprintf("http://localhost:%d", etcdPeerPort)
	etcdDataDir, err := ioutil.TempDir("", "etcd-data")
	Expect(err).ToNot(HaveOccurred())

	etcdCommand := exec.Command(
		etcdPath,
		"--data-dir", etcdDataDir,
		"--listen-client-urls", etcdClientURL,
		"--listen-peer-urls", etcdPeerURL,
		"--advertise-client-urls", etcdClientURL,
	)
	etcdSession, err := gexec.Start(
		etcdCommand,
		gexec.NewPrefixedWriter(fmt.Sprintf(stdOut, yellow, "etcd"), GinkgoWriter),
		gexec.NewPrefixedWriter(fmt.Sprintf(stdErr, yellow, "etcd"), GinkgoWriter),
	)
	Expect(err).ToNot(HaveOccurred())

	By("waiting for etcd to respond via http")
	Eventually(func() error {
		req, reqErr := http.NewRequest("PUT", etcdClientURL+"/v2/keys/test", strings.NewReader("value=test"))
		if reqErr != nil {
			return reqErr
		}
		resp, reqErr := http.DefaultClient.Do(req)
		if reqErr != nil {
			return reqErr
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusInternalServerError {
			return errors.New(fmt.Sprintf("got %d response from etcd", resp.StatusCode))
		}
		return nil
	}, 10).Should(Succeed())

	return func() {
		os.RemoveAll(etcdDataDir)
		etcdSession.Kill().Wait()
	}, etcdClientURL
}

func SetupDoppler(etcdClientURL string) (func(), int) {
	By("making sure doppler was build")
	dopplerPath := os.Getenv("DOPPLER_BUILD_PATH")
	Expect(dopplerPath).ToNot(BeEmpty())

	By("starting doppler")
	dopplerUDPPort := getPort(dopplerUDPPortOffset)
	dopplerTCPPort := getPort(dopplerTCPPortOffset)
	dopplerTLSPort := getPort(dopplerTLSPortOffset)
	dopplerOutgoingPort := getPort(dopplerOutgoingPortOffset)

	dopplerConf := dopplerConfig.Config{
		IncomingUDPPort:    uint32(dopplerUDPPort),
		IncomingTCPPort:    uint32(dopplerTCPPort),
		OutgoingPort:       uint32(dopplerOutgoingPort),
		EtcdUrls:           []string{etcdClientURL},
		EnableTLSTransport: true,
		TLSListenerConfig: dopplerConfig.TLSListenerConfig{
			Port: uint32(dopplerTLSPort),
			// TODO: move these files as source code and write them to tmp files
			CertFile: "../fixtures/server.crt",
			KeyFile:  "../fixtures/server.key",
			CAFile:   "../fixtures/loggregator-ca.crt",
		},
		MaxRetainedLogMessages:       10,
		MessageDrainBufferSize:       100,
		SinkDialTimeoutSeconds:       10,
		SinkIOTimeoutSeconds:         10,
		SinkInactivityTimeoutSeconds: 10,
		UnmarshallerCount:            5,
		Index:                        jobIndex,
		JobName:                      jobName,
		SharedSecret:                 sharedSecret,
		Zone:                         availabilityZone,
	}

	dopplerCfgFile, err := ioutil.TempFile("", "doppler-config")
	Expect(err).ToNot(HaveOccurred())

	err = json.NewEncoder(dopplerCfgFile).Encode(dopplerConf)
	Expect(err).ToNot(HaveOccurred())
	err = dopplerCfgFile.Close()
	Expect(err).ToNot(HaveOccurred())

	dopplerCommand := exec.Command(dopplerPath, "--config", dopplerCfgFile.Name())
	dopplerSession, err := gexec.Start(
		dopplerCommand,
		gexec.NewPrefixedWriter(fmt.Sprintf(stdOut, blue, "doppler"), GinkgoWriter),
		gexec.NewPrefixedWriter(fmt.Sprintf(stdErr, blue, "doppler"), GinkgoWriter),
	)
	Expect(err).ToNot(HaveOccurred())

	// a terrible hack
	Eventually(dopplerSession.Buffer).Should(gbytes.Say("doppler server started"))

	By("waiting for doppler to listen")
	Eventually(func() error {
		c, reqErr := net.Dial("tcp", fmt.Sprintf(":%d", dopplerOutgoingPort))
		if reqErr == nil {
			c.Close()
		}
		return reqErr
	}, 3).Should(Succeed())

	return func() {
		os.Remove(dopplerCfgFile.Name())
		dopplerSession.Kill().Wait()
	}, dopplerOutgoingPort
}

func SetupMetron(etcdClientURL, proto string) (func(), int) {
	By("making sure metron was build")
	metronPath := os.Getenv("METRON_BUILD_PATH")
	Expect(metronPath).ToNot(BeEmpty())

	By("starting metron")
	protocols := []metronConfig.Protocol{metronConfig.Protocol(proto)}
	metronPort := getPort(metronPortOffset)
	metronConf := metronConfig.Config{
		Deployment:                       "deployment",
		Zone:                             availabilityZone,
		Job:                              jobName,
		Index:                            jobIndex,
		IncomingUDPPort:                  metronPort,
		EtcdUrls:                         []string{etcdClientURL},
		SharedSecret:                     sharedSecret,
		MetricBatchIntervalMilliseconds:  10,
		RuntimeStatsIntervalMilliseconds: 10,
		EtcdMaxConcurrentRequests:        10,
		Protocols:                        metronConfig.Protocols(protocols),
	}

	switch proto {
	case "udp":
	case "tls":
		metronConf.TLSConfig = metronConfig.TLSConfig{
			CertFile: "../fixtures/client.crt",
			KeyFile:  "../fixtures/client.key",
			CAFile:   "../fixtures/loggregator-ca.crt",
		}
		fallthrough
	case "tcp":
		metronConf.TCPBatchIntervalMilliseconds = 100
		metronConf.TCPBatchSizeBytes = 10240
	}

	metronCfgFile, err := ioutil.TempFile("", "metron-config")
	Expect(err).ToNot(HaveOccurred())

	err = json.NewEncoder(metronCfgFile).Encode(metronConf)
	Expect(err).ToNot(HaveOccurred())
	err = metronCfgFile.Close()
	Expect(err).ToNot(HaveOccurred())

	metronCommand := exec.Command(metronPath, "--debug", "--config", metronCfgFile.Name())
	metronSession, err := gexec.Start(
		metronCommand,
		gexec.NewPrefixedWriter(fmt.Sprintf(stdOut, magenta, "metron"), GinkgoWriter),
		gexec.NewPrefixedWriter(fmt.Sprintf(stdErr, magenta, "metron"), GinkgoWriter),
	)
	Expect(err).ToNot(HaveOccurred())

	Eventually(metronSession.Buffer).Should(gbytes.Say(" from last etcd event, updating writer..."))

	By("waiting for metron to listen")
	Eventually(func() error {
		c, reqErr := net.Dial("udp4", fmt.Sprintf(":%d", metronPort))
		if reqErr == nil {
			c.Close()
		}
		return reqErr
	}, 3).Should(Succeed())

	return func() {
		os.Remove(metronCfgFile.Name())
		metronSession.Kill().Wait()
	}, metronPort
}

func SetupTrafficcontroller(etcdClientURL string, dopplerPort, metronPort int) (func(), int) {
	By("making sure trafficcontroller was build")
	tcPath := os.Getenv("TRAFFIC_CONTROLLER_BUILD_PATH")
	Expect(tcPath).ToNot(BeEmpty())

	By("starting trafficcontroller")
	tcPort := getPort(trafficcontrollerPortOffset)
	tcConfig := trafficcontrollerConfig.Config{
		EtcdUrls:                  []string{etcdClientURL},
		EtcdMaxConcurrentRequests: 10,
		JobName:                   jobName,
		Index:                     jobIndex,
		DopplerPort:               uint32(dopplerPort),
		OutgoingDropsondePort:     uint32(tcPort),
		MetronHost:                "localhost",
		MetronPort:                metronPort,
		SystemDomain:              "vcap.me",
		SkipCertVerify:            true,
	}

	tcCfgFile, err := ioutil.TempFile("", "trafficcontroller-config")
	Expect(err).ToNot(HaveOccurred())

	err = json.NewEncoder(tcCfgFile).Encode(tcConfig)
	Expect(err).ToNot(HaveOccurred())
	err = tcCfgFile.Close()
	Expect(err).ToNot(HaveOccurred())

	tcCommand := exec.Command(tcPath, "--debug", "--disableAccessControl", "--config", tcCfgFile.Name())
	tcSession, err := gexec.Start(
		tcCommand,
		gexec.NewPrefixedWriter(fmt.Sprintf(stdOut, cyan, "tc"), GinkgoWriter),
		gexec.NewPrefixedWriter(fmt.Sprintf(stdErr, cyan, "tc"), GinkgoWriter),
	)
	Expect(err).ToNot(HaveOccurred())

	By("waiting for trafficcontroller to listen")
	ip, err := localip.LocalIP()
	Expect(err).ToNot(HaveOccurred())
	Eventually(func() error {
		c, reqErr := net.Dial("tcp", fmt.Sprintf("%s:%d", ip, tcPort))
		if reqErr == nil {
			c.Close()
		}
		return reqErr
	}, 3).Should(Succeed())

	return func() {
		os.Remove(tcCfgFile.Name())
		tcSession.Kill().Wait()
	}, tcPort
}
