package __latency

import (
	"context"
	"fmt"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	performancev2 "github.com/openshift-kni/performance-addon-operators/api/v2"
	testutils "github.com/openshift-kni/performance-addon-operators/functests/utils"
	testclient "github.com/openshift-kni/performance-addon-operators/functests/utils/client"
	"github.com/openshift-kni/performance-addon-operators/functests/utils/discovery"
	"github.com/openshift-kni/performance-addon-operators/functests/utils/images"
	testlog "github.com/openshift-kni/performance-addon-operators/functests/utils/log"
	"github.com/openshift-kni/performance-addon-operators/functests/utils/nodes"
	"github.com/openshift-kni/performance-addon-operators/functests/utils/pods"
	"github.com/openshift-kni/performance-addon-operators/functests/utils/profiles"
	"github.com/openshift-kni/performance-addon-operators/pkg/controller/performanceprofile/components"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"k8s.io/utils/pointer"
)

const (
	oslatTestName       = "oslat"
	cyclictestTestName  = "cyclictest"
	hwlatdetectTestName = "hwlatdetect"
	defaultTestDelay    = 0
	defaultTestRun      = false
	defaultTestRuntime  = "300"
	defaultMaxLatency   = -1
	defaultTestCpus     = -1
)

var (
	latencyTestDelay   = defaultTestDelay
	latencyTestRun     = defaultTestRun
	latencyTestRuntime = defaultTestRuntime
	maximumLatency     = defaultMaxLatency
	latencyTestCpus    = defaultTestCpus
)

// LATENCY_TEST_DELAY delay the run of the binary, can be useful to give time to the CPU manager reconcile loop
// to update the default CPU pool
// LATENCY_TEST_RUN: indicates if the latency test should run
// LATENCY_TEST_RUNTIME: the amount of time in seconds that the latency test should run
// LATENCY_TEST_CPUS: the amount of CPUs the pod which run the latency test should request

var _ = Describe("[performance] Latency Test", func() {
	var workerRTNode *corev1.Node
	var profile *performancev2.PerformanceProfile
	var latencyTestPod *corev1.Pod
	var err error

	BeforeEach(func() {
		latencyTestRun, err = getLatencyTestRun()
		Expect(err).ToNot(HaveOccurred())

		latencyTestDelay, err = getLatencyTestDelay()
		Expect(err).ToNot(HaveOccurred())

		latencyTestCpus, err = getLatencyTestCpus()
		Expect(err).ToNot(HaveOccurred())

		latencyTestRuntime = getLatencyTestRuntime()

		if !latencyTestRun {
			Skip("Skip the latency test, the LATENCY_TEST_RUN set to false")
		}

		if discovery.Enabled() && testutils.ProfileNotFound {
			Skip("Discovery mode enabled, performance profile not found")
		}

		profile, err = profiles.GetByNodeLabels(testutils.NodeSelectorLabels)
		Expect(err).ToNot(HaveOccurred())

		workerRTNodes, err := nodes.GetByLabels(testutils.NodeSelectorLabels)
		Expect(err).ToNot(HaveOccurred())

		workerRTNodes, err = nodes.MatchingOptionalSelector(workerRTNodes)
		Expect(err).ToNot(HaveOccurred(), "error looking for the optional selector: %v", err)

		Expect(workerRTNodes).ToNot(BeEmpty())
		workerRTNode = &workerRTNodes[0]
	})

	AfterEach(func() {
		err = testclient.Client.Delete(context.TODO(), latencyTestPod)
		if err != nil {
			testlog.Error(err)
		}

		err = pods.WaitForDeletion(latencyTestPod, pods.DefaultDeletionTimeout*time.Second)
		if err != nil {
			testlog.Error(err)
		}

		maximumLatency = -1
	})

	Context("with the oslat image", func() {
		testName := oslatTestName

		BeforeEach(func() {
			maximumLatency, err = getMaximumLatency(testName)
			Expect(err).ToNot(HaveOccurred())

			if profile.Spec.CPU.Isolated == nil {
				Skip(fmt.Sprintf("Skip the oslat test, the profile %q does not have isolated CPUs", profile.Name))
			}

			isolatedCpus := cpuset.MustParse(string(*profile.Spec.CPU.Isolated))
			// we require at least two CPUs to run oslat test, because one CPU should be used to run the main oslat thread
			// we can not use all isolated CPUs, because if reserved and isolated include all node CPUs, and reserved CPUs
			// do not calculated into the Allocated, at least part of time of one of isolated CPUs will be used to run
			// other node containers
			// at least two isolated CPUs to run oslat + one isolated CPU used by other containers on the node = at least 3 isolated CPUs
			if isolatedCpus.Size() < 3 {
				Skip(fmt.Sprintf("Skip the oslat test, the profile %q has less than two isolated CPUs", profile.Name))
			}
		})

		It("should succeed", func() {
			oslatArgs := []string{
				fmt.Sprintf("-runtime=%s", latencyTestRuntime),
			}
			latencyTestPod = getLatencyTestPod(profile, workerRTNode, testName, oslatArgs)
			createLatencyTestPod(latencyTestPod)

			// verify the maximum latency only when it requested, because this value can be very different
			// on different systems
			if maximumLatency == -1 {
				Skip("no maximum latency value provided, skip buckets latency check")
			}

			latencies := extractLatencyValues("oslat", `Maximum:\t*([\s\d]*)\(us\)`, workerRTNode)

			// under the output of the oslat very often we have one anomaly high value, for example
			// Maximum:    16543 15 15 14 13 12 12 13 12 12 12 12 12 12 12 12 12 (us)
			// it still unclear if it oslat bug or the kernel one, but we definitely do not want to
			// fail our test on it
			var anomaly bool
			for _, lat := range strings.Split(latencies, " ") {
				if lat == "" {
					continue
				}

				curr, err := strconv.Atoi(lat)
				Expect(err).ToNot(HaveOccurred())

				// skip the anomaly value
				if curr > maximumLatency && !anomaly {
					anomaly = true
					continue
				}

				Expect(curr < maximumLatency).To(BeTrue(), "The current latency %d is bigger than the expected one %d", curr, maximumLatency)
			}
		})
	})

	Context("with the cyclictest image", func() {
		testName := cyclictestTestName

		BeforeEach(func() {
			maximumLatency, err = getMaximumLatency(testName)
			Expect(err).ToNot(HaveOccurred())

			if profile.Spec.CPU.Isolated == nil {
				Skip(fmt.Sprintf("Skip the cyclictest test, the profile %q does not have isolated CPUs", profile.Name))
			}
		})

		It("should succeed", func() {
			latencyTestPod = getLatencyTestPod(profile, workerRTNode, testName, []string{})
			createLatencyTestPod(latencyTestPod)

			// verify the maximum latency only when it requested, because this value can be very different
			// on different systems
			if maximumLatency == -1 {
				Skip("no maximum latency value provided, skip buckets latency check")
			}

			latencies := extractLatencyValues("cyclictest", `# Max Latencies:\t*\s*(.*)\s*\t*`, workerRTNode)
			for _, lat := range strings.Split(latencies, " ") {
				if lat == "" {
					continue
				}

				curr, err := strconv.Atoi(lat)
				Expect(err).ToNot(HaveOccurred())

				Expect(curr < maximumLatency).To(BeTrue(), "The current latency %d is bigger than the expected one %d", curr, maximumLatency)
			}
		})
	})

	Context("with the hwlatdetect image", func() {
		testName := hwlatdetectTestName

		BeforeEach(func() {
			maximumLatency, err = getMaximumLatency(testName)
			Expect(err).ToNot(HaveOccurred())
		})

		It("should succeed", func() {
			hardLimit := maximumLatency
			if hardLimit == -1 {
				// This value should be > than max latency,
				// in order to prevent the hwlatdetect return with error 1 in case latency value is bigger than expected.
				// in case latency value is bigger than expected, it will be handled on different flow.
				hardLimit = 1000
			}

			hwlatdetectArgs := []string{
				fmt.Sprintf("-hardlimit=%d", hardLimit),
			}

			// set the maximum latency for the test if needed
			if maximumLatency != -1 {
				hwlatdetectArgs = append(hwlatdetectArgs, fmt.Sprintf("-threshold=%d", maximumLatency))
			}

			latencyTestPod = getLatencyTestPod(profile, workerRTNode, testName, hwlatdetectArgs)
			createLatencyTestPod(latencyTestPod)
			// here we don't need to parse the latency values.
			// hwlatdetect will do that for us and exit with error if needed.
		})
	})
})

func getLatencyTestRun() (bool, error) {
	if latencyTestRunEnv, ok := os.LookupEnv("LATENCY_TEST_RUN"); ok {
		return strconv.ParseBool(latencyTestRunEnv)
	}
	return defaultTestRun, nil
}

func getLatencyTestRuntime() string {
	if latencyTestRuntimeEnv, ok := os.LookupEnv("LATENCY_TEST_RUNTIME"); ok {
		return latencyTestRuntimeEnv
	}
	return defaultTestRuntime
}

func getLatencyTestDelay() (int, error) {
	if latencyTestDelayEnv, ok := os.LookupEnv("LATENCY_TEST_DELAY"); ok {
		if val, err := strconv.Atoi(latencyTestDelayEnv); err != nil {
			return val, fmt.Errorf("the environment variable LATENCY_TEST_DELAY has incorrect value %q: %w", latencyTestDelayEnv, err)
		} else {
			return val, nil
		}
	}
	return defaultTestDelay, nil
}

func getLatencyTestCpus() (int, error) {
	if latencyTestCpusEnv, ok := os.LookupEnv("LATENCY_TEST_CPUS"); ok {
		if val, err := strconv.Atoi(latencyTestCpusEnv); err != nil {
			return val, fmt.Errorf("the environment variable LATENCY_TEST_CPUS has incorrect value %q: %w", latencyTestCpusEnv, err)
		} else {
			return val, nil
		}
	}
	return defaultTestCpus, nil
}

// getMaximumLatency should look for one of the following environment variables:
// OSLAT_MAXIMUM_LATENCY: the expected maximum latency for all buckets in us
// CYCLICTEST_MAXIMUM_LATENCY: the expected maximum latency for all buckets in us
// HWLATDETECT_MAXIMUM_LATENCY: the expected maximum latency for all buckets in us
// MAXIMUM_LATENCY: unified expected maximum latency for all tests
func getMaximumLatency(testName string) (int, error) {
	var err error
	val := defaultMaxLatency
	if unifiedMaxLatencyEnv, ok := os.LookupEnv("MAXIMUM_LATENCY"); ok {
		if val, err = strconv.Atoi(unifiedMaxLatencyEnv); err != nil {
			err = fmt.Errorf("the environment variable MAXIMUM_LATENCY has incorrect value %q: %w", unifiedMaxLatencyEnv, err)
			return val, err
		}
	}

	// specific values will have precedence over the general one
	envVariableName := fmt.Sprintf("%s_MAXIMUM_LATENCY", strings.ToUpper(testName))
	if maximumLatencyEnv, ok := os.LookupEnv(envVariableName); ok {
		if val, err = strconv.Atoi(maximumLatencyEnv); err != nil {
			err = fmt.Errorf("the environment variable %q has incorrect value %q: %w", envVariableName, maximumLatencyEnv, err)
		}
	}
	return val, err
}

func getLatencyTestPod(profile *performancev2.PerformanceProfile, node *corev1.Node, testName string, testSpecificArgs []string) *corev1.Pod {
	runtimeClass := components.GetComponentName(profile.Name, components.ComponentNamePrefix)
	testNamePrefix := fmt.Sprintf("%s-", testName)
	runnerName := fmt.Sprintf("%srunner", testNamePrefix)
	runnerPath := path.Join("usr", "bin", runnerName)

	if latencyTestCpus == -1 {
		// we can not use all isolated CPUs, because if reserved and isolated include all node CPUs, and reserved CPUs
		// do not calculated into the Allocated, at least part of time of one of isolated CPUs will be used to run
		// other node containers
		cpus := cpuset.MustParse(string(*profile.Spec.CPU.Isolated))
		latencyTestCpus = cpus.Size() - 1
	}

	latencyTestRunnerArgs := []string{
		"-logtostderr=false",
		"-alsologtostderr=true",
		fmt.Sprintf("-log_file=/host/%s.log", testName),
	}

	latencyTestRunnerArgs = append(latencyTestRunnerArgs, testSpecificArgs...)

	if latencyTestDelay > 0 {
		latencyTestRunnerArgs = append(latencyTestRunnerArgs, fmt.Sprintf("-%s-start-delay=%d", testName, latencyTestDelay))
	}

	volumeTypeDirectory := corev1.HostPathDirectory
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: testNamePrefix,
			Annotations: map[string]string{
				"irq-load-balancing.crio.io": "disable",
				"cpu-load-balancing.crio.io": "disable",
			},
			Namespace: testutils.NamespaceTesting,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:    corev1.RestartPolicyNever,
			RuntimeClassName: &runtimeClass,
			Containers: []corev1.Container{
				{
					Name:  runnerName,
					Image: images.Test(),
					Command: []string{
						runnerPath,
					},
					Args: latencyTestRunnerArgs,
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(strconv.Itoa(latencyTestCpus)),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
					SecurityContext: &corev1.SecurityContext{
						Privileged: pointer.BoolPtr(true),
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "logs",
							MountPath: "/host",
						},
					},
				},
			},
			NodeSelector: map[string]string{
				"kubernetes.io/hostname": node.Labels["kubernetes.io/hostname"],
			},
			Volumes: []corev1.Volume{
				{
					Name: "logs",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/var/log",
							Type: &volumeTypeDirectory,
						},
					},
				},
			},
		},
	}
}

func createLatencyTestPod(testPod *corev1.Pod) {
	err := testclient.Client.Create(context.TODO(), testPod)
	Expect(err).ToNot(HaveOccurred())

	timeout, err := strconv.Atoi(latencyTestRuntime)
	Expect(err).ToNot(HaveOccurred())

	By("Waiting two minutes to download the latencyTest image")
	err = pods.WaitForPhase(testPod, corev1.PodRunning, 2*time.Minute)
	Expect(err).ToNot(HaveOccurred())

	By("Waiting another two minutes to give enough time for the cluster to move the pod to Succeeded phase")
	podTimeout := time.Duration(timeout + 120)
	err = pods.WaitForPhase(testPod, corev1.PodSucceeded, podTimeout*time.Second)
	Expect(err).ToNot(HaveOccurred())
}

func extractLatencyValues(testName string, exp string, node *corev1.Node) string {
	cmd := []string{"cat", fmt.Sprintf("/rootfs/var/log/%s.log", testName)}
	out, err := nodes.ExecCommandOnNode(cmd, node)
	Expect(err).ToNot(HaveOccurred())

	maximumRegex, err := regexp.Compile(exp)
	Expect(err).ToNot(HaveOccurred())

	latencies := maximumRegex.FindStringSubmatch(out)
	Expect(len(latencies)).To(Equal(2))

	return latencies[1]
}
