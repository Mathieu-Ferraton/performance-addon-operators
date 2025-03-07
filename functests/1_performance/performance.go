package __performance

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/api/node/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/controller-runtime/pkg/client"

	tunedv1 "github.com/openshift/cluster-node-tuning-operator/pkg/apis/tuned/v1"
	machineconfigv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	mcov1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"

	performancev1 "github.com/openshift-kni/performance-addon-operators/api/v1"
	performancev1alpha1 "github.com/openshift-kni/performance-addon-operators/api/v1alpha1"
	performancev2 "github.com/openshift-kni/performance-addon-operators/api/v2"
	testutils "github.com/openshift-kni/performance-addon-operators/functests/utils"
	testclient "github.com/openshift-kni/performance-addon-operators/functests/utils/client"
	"github.com/openshift-kni/performance-addon-operators/functests/utils/cluster"
	"github.com/openshift-kni/performance-addon-operators/functests/utils/discovery"
	testlog "github.com/openshift-kni/performance-addon-operators/functests/utils/log"
	"github.com/openshift-kni/performance-addon-operators/functests/utils/mcps"
	"github.com/openshift-kni/performance-addon-operators/functests/utils/nodes"
	"github.com/openshift-kni/performance-addon-operators/functests/utils/pods"
	"github.com/openshift-kni/performance-addon-operators/functests/utils/profiles"
	"github.com/openshift-kni/performance-addon-operators/pkg/controller/performanceprofile/components"
	"github.com/openshift-kni/performance-addon-operators/pkg/controller/performanceprofile/components/machineconfig"
	componentprofile "github.com/openshift-kni/performance-addon-operators/pkg/controller/performanceprofile/components/profile"
)

const (
	testTimeout      = 480
	testPollInterval = 2
)

var RunningOnSingleNode bool

var _ = Describe("[rfe_id:27368][performance]", func() {
	var workerRTNodes []corev1.Node
	var profile *performancev2.PerformanceProfile

	testutils.BeforeAll(func() {
		isSNO, err := cluster.IsSingleNode()
		Expect(err).ToNot(HaveOccurred())
		RunningOnSingleNode = isSNO
	})

	BeforeEach(func() {
		if discovery.Enabled() && testutils.ProfileNotFound {
			Skip("Discovery mode enabled, performance profile not found")
		}

		var err error
		workerRTNodes, err = nodes.GetByLabels(testutils.NodeSelectorLabels)
		Expect(err).ToNot(HaveOccurred(), "error looking for node with role %q: %v", testutils.RoleWorkerCNF, err)
		workerRTNodes, err = nodes.MatchingOptionalSelector(workerRTNodes)
		Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("error looking for the optional selector: %v", err))
		Expect(workerRTNodes).ToNot(BeEmpty(), "no nodes with role %q found", testutils.RoleWorkerCNF)
		profile, err = profiles.GetByNodeLabels(testutils.NodeSelectorLabels)
		Expect(err).ToNot(HaveOccurred(), "cannot get profile by node labels %v", testutils.NodeSelectorLabels)
	})

	// self-tests; these are only vaguely related to performance becase these are enablement conditions, not actual settings.
	// For example, running on control plane means we leave more resources for the workload.
	Context("Performance Operator", func() {
		It("[test_id:38109] Should run on the control plane nodes", func() {
			pod, err := pods.GetPerformanceOperatorPod()
			Expect(err).ToNot(HaveOccurred(), "Failed to find the Performance Addon Operator pod")

			Expect(strings.HasPrefix(pod.Name, "performance-operator")).To(BeTrue(),
				"Performance Addon Operator pod name should start with performance-operator prefix")

			masterNodes, err := nodes.GetByRole(testutils.RoleMaster)
			Expect(err).ToNot(HaveOccurred(), "Failed to query the master nodes")
			for _, node := range masterNodes {
				if node.Name == pod.Spec.NodeName {
					return
				}
			}

			Fail("Performance Addon Operator is not running in a master node")
		})
	})

	Context("Tuned CRs generated from profile", func() {
		tunedExpectedName := components.GetComponentName(testutils.PerformanceProfileName, components.ProfileNamePerformance)
		It("[test_id:31748] Should have the expected name for tuned from the profile owner object", func() {
			tunedList := &tunedv1.TunedList{}
			key := types.NamespacedName{
				Name:      tunedExpectedName,
				Namespace: components.NamespaceNodeTuningOperator,
			}
			tuned := &tunedv1.Tuned{}
			err := testclient.Client.Get(context.TODO(), key, tuned)
			Expect(err).ToNot(HaveOccurred(), "cannot find the Cluster Node Tuning Operator object %q", tuned.Name)

			Eventually(func() bool {
				err := testclient.Client.List(context.TODO(), tunedList)
				Expect(err).NotTo(HaveOccurred())
				for t := range tunedList.Items {
					tunedItem := tunedList.Items[t]
					ownerReferences := tunedItem.ObjectMeta.OwnerReferences
					for o := range ownerReferences {
						if ownerReferences[o].Name == profile.Name && tunedItem.Name != tunedExpectedName {
							return false
						}
					}
				}
				return true
			}, cluster.ComputeTestTimeout(120*time.Second, RunningOnSingleNode), testPollInterval*time.Second).Should(BeTrue(),
				"tuned CR name owned by a performance profile CR should only be %q", tunedExpectedName)
		})

		It("[test_id:37127] Node should point to right tuned profile", func() {
			for _, node := range workerRTNodes {
				tuned := tunedForNode(&node)
				activeProfile, err := pods.ExecCommandOnPod(testclient.K8sClient, tuned, []string{"cat", "/etc/tuned/active_profile"})
				Expect(err).ToNot(HaveOccurred(), "Error getting the tuned active profile")
				activeProfileName := string(activeProfile)
				Expect(strings.TrimSpace(activeProfileName)).To(Equal(tunedExpectedName), "active profile name mismatch got %q expected %q", activeProfileName, tunedExpectedName)
			}
		})
	})

	Context("Pre boot tuning adjusted by tuned ", func() {

		It("[test_id:31198] Should set CPU affinity kernel argument", func() {
			for _, node := range workerRTNodes {
				cmdline, err := nodes.ExecCommandOnMachineConfigDaemon(&node, []string{"cat", "/proc/cmdline"})
				Expect(err).ToNot(HaveOccurred())
				// since systemd.cpu_affinity is calculated on node level using tuned we can check only the key in this context.
				Expect(string(cmdline)).To(ContainSubstring("systemd.cpu_affinity="))
			}
		})

		It("[test_id:32702] Should set CPU isolcpu's kernel argument managed_irq flag", func() {
			for _, node := range workerRTNodes {
				cmdline, err := nodes.ExecCommandOnMachineConfigDaemon(&node, []string{"cat", "/proc/cmdline"})
				Expect(err).ToNot(HaveOccurred())
				if profile.Spec.CPU.BalanceIsolated != nil && *profile.Spec.CPU.BalanceIsolated == false {
					Expect(string(cmdline)).To(ContainSubstring("isolcpus=domain,managed_irq,"))
				} else {
					Expect(string(cmdline)).To(ContainSubstring("isolcpus=managed_irq,"))
				}
			}
		})

		It("[test_id:27081][crit:high][vendor:cnf-qe@redhat.com][level:acceptance] Should set workqueue CPU mask", func() {
			for _, node := range workerRTNodes {
				By(fmt.Sprintf("Getting tuned.non_isolcpus kernel argument on %q", node.Name))
				cmdline, err := nodes.ExecCommandOnMachineConfigDaemon(&node, []string{"cat", "/proc/cmdline"})
				Expect(err).ToNot(HaveOccurred())
				re := regexp.MustCompile(`tuned.non_isolcpus=\S+`)
				nonIsolcpusFullArgument := re.FindString(string(cmdline))
				Expect(nonIsolcpusFullArgument).To(ContainSubstring("tuned.non_isolcpus="), "tuned.non_isolcpus parameter not found in %q", cmdline)
				nonIsolcpusMask := strings.Split(string(nonIsolcpusFullArgument), "=")[1]
				nonIsolcpusMaskNoDelimiters := strings.Replace(nonIsolcpusMask, ",", "", -1)

				getTrimmedMaskFromData := func(maskType string, data []byte) string {
					trimmed := strings.TrimSpace(string(data))
					testlog.Infof("workqueue %s mask for %q: %q", maskType, node.Name, trimmed)
					return strings.Replace(trimmed, ",", "", -1)
				}

				expectMasksEqual := func(expected, got string) {
					expectedTrimmed := strings.TrimLeft(expected, "0")
					gotTrimmed := strings.TrimLeft(got, "0")
					ExpectWithOffset(1, expectedTrimmed).Should(Equal(gotTrimmed), "wrong workqueue mask on %q - got %q (from %q) expected %q (from %q)", node.Name, expectedTrimmed, expected, got, gotTrimmed)
				}

				By(fmt.Sprintf("Getting the virtual workqueue mask (/sys/devices/virtual/workqueue/cpumask) on %q", node.Name))
				workqueueMaskData, err := nodes.ExecCommandOnMachineConfigDaemon(&node, []string{"cat", "/sys/devices/virtual/workqueue/cpumask"})
				Expect(err).ToNot(HaveOccurred())
				workqueueMask := getTrimmedMaskFromData("virtual", workqueueMaskData)
				expectMasksEqual(nonIsolcpusMaskNoDelimiters, workqueueMask)

				By(fmt.Sprintf("Getting the writeback workqueue mask (/sys/bus/workqueue/devices/writeback/cpumask) on %q", node.Name))
				workqueueWritebackMaskData, err := nodes.ExecCommandOnMachineConfigDaemon(&node, []string{"cat", "/sys/bus/workqueue/devices/writeback/cpumask"})
				Expect(err).ToNot(HaveOccurred())
				workqueueWritebackMask := getTrimmedMaskFromData("workqueue", workqueueWritebackMaskData)
				expectMasksEqual(nonIsolcpusMaskNoDelimiters, workqueueWritebackMask)
			}
		})

		It("[test_id:32375][crit:high][vendor:cnf-qe@redhat.com][level:acceptance] initramfs should not have injected configuration", func() {
			for _, node := range workerRTNodes {
				rhcosId, err := nodes.ExecCommandOnMachineConfigDaemon(&node, []string{"awk", "-F", "/", "{printf $3}", "/rootfs/proc/cmdline"})
				Expect(err).ToNot(HaveOccurred())
				initramfsImagesPath, err := nodes.ExecCommandOnMachineConfigDaemon(&node, []string{"find", filepath.Join("/rootfs/boot/ostree", string(rhcosId)), "-name", "*.img"})
				Expect(err).ToNot(HaveOccurred())
				modifiedImagePath := strings.TrimPrefix(strings.TrimSpace(string(initramfsImagesPath)), "/rootfs")
				initrd, err := nodes.ExecCommandOnMachineConfigDaemon(&node, []string{"chroot", "/rootfs", "lsinitrd", modifiedImagePath})
				Expect(err).ToNot(HaveOccurred())
				Expect(string(initrd)).ShouldNot(ContainSubstring("'/etc/systemd/system.conf /etc/systemd/system.conf.d/setAffinity.conf'"))
			}
		})

		It("[test_id:35363][crit:high][vendor:cnf-qe@redhat.com][level:acceptance] stalld daemon is running on the host", func() {
			for _, node := range workerRTNodes {
				tuned := tunedForNode(&node)
				_, err := pods.ExecCommandOnPod(testclient.K8sClient, tuned, []string{"pidof", "stalld"})
				Expect(err).ToNot(HaveOccurred())
			}
		})
		It("[test_id:42400][crit:medium][vendor:cnf-qe@redhat.com][level:acceptance] stalld daemon is running as sched_fifo", func() {
			for _, node := range workerRTNodes {
				pid, err := nodes.ExecCommandOnNode([]string{"pidof", "stalld"}, &node)
				Expect(err).ToNot(HaveOccurred())
				Expect(pid).ToNot(BeEmpty())
				sched_tasks, err := nodes.ExecCommandOnNode([]string{"chrt", "-ap", pid}, &node)
				Expect(err).ToNot(HaveOccurred())
				Expect(sched_tasks).To(ContainSubstring("scheduling policy: SCHED_FIFO"))
				Expect(sched_tasks).To(ContainSubstring("scheduling priority: 10"))
			}
		})
		It("[test_id:42696][crit:medium][vendor:cnf-qe@redhat.com][level:acceptance] Stalld runs in higher priority than ksoftirq and rcu{c,b}", func() {
			for _, node := range workerRTNodes {
				stalld_pid, err := nodes.ExecCommandOnNode([]string{"pidof", "stalld"}, &node)
				Expect(err).ToNot(HaveOccurred())
				Expect(stalld_pid).ToNot(BeEmpty())
				sched_tasks, err := nodes.ExecCommandOnNode([]string{"chrt", "-ap", stalld_pid}, &node)
				Expect(err).ToNot(HaveOccurred())
				re := regexp.MustCompile("scheduling priority: ([0-9]+)")
				match := re.FindStringSubmatch(sched_tasks)
				stalld_prio, err := strconv.Atoi(match[1])
				Expect(err).ToNot(HaveOccurred())

				ksoftirq_pid, err := nodes.ExecCommandOnNode([]string{"pgrep", "-f", "ksoftirqd", "-n"}, &node)
				Expect(err).ToNot(HaveOccurred())
				Expect(ksoftirq_pid).ToNot(BeEmpty())
				sched_tasks, err = nodes.ExecCommandOnNode([]string{"chrt", "-ap", ksoftirq_pid}, &node)
				Expect(err).ToNot(HaveOccurred())
				match = re.FindStringSubmatch(sched_tasks)
				ksoftirq_prio, err := strconv.Atoi(match[1])
				Expect(err).ToNot(HaveOccurred())

				rcuc_pid, err := nodes.ExecCommandOnNode([]string{"pgrep", "-f", "rcuc", "-n"}, &node)
				Expect(err).ToNot(HaveOccurred())
				Expect(rcuc_pid).ToNot(BeEmpty())
				sched_tasks, err = nodes.ExecCommandOnNode([]string{"chrt", "-ap", rcuc_pid}, &node)
				Expect(err).ToNot(HaveOccurred())
				match = re.FindStringSubmatch(sched_tasks)
				rcuc_prio, err := strconv.Atoi(match[1])
				Expect(err).ToNot(HaveOccurred())

				Expect(stalld_prio).To(BeNumerically("<", ksoftirq_prio))
				Expect(stalld_prio).To(BeNumerically("<", rcuc_prio))
			}
		})

	})

	Context("Additional kernel arguments added from perfomance profile", func() {
		It("[test_id:28611][crit:high][vendor:cnf-qe@redhat.com][level:acceptance] Should set additional kernel arguments on the machine", func() {
			if profile.Spec.AdditionalKernelArgs != nil {
				for _, node := range workerRTNodes {
					cmdline, err := nodes.ExecCommandOnMachineConfigDaemon(&node, []string{"cat", "/proc/cmdline"})
					Expect(err).ToNot(HaveOccurred())
					for _, arg := range profile.Spec.AdditionalKernelArgs {
						Expect(string(cmdline)).To(ContainSubstring(arg))
					}
				}
			}
		})
	})

	Context("Tuned kernel parameters", func() {
		It("[test_id:28466][crit:high][vendor:cnf-qe@redhat.com][level:acceptance] Should contain configuration injected through openshift-node-performance profile", func() {
			sysctlMap := map[string]string{
				"kernel.hung_task_timeout_secs": "600",
				"kernel.nmi_watchdog":           "0",
				"kernel.sched_rt_runtime_us":    "-1",
				"vm.stat_interval":              "10",
				"kernel.timer_migration":        "0",
			}

			key := types.NamespacedName{
				Name:      components.GetComponentName(testutils.PerformanceProfileName, components.ProfileNamePerformance),
				Namespace: components.NamespaceNodeTuningOperator,
			}
			tuned := &tunedv1.Tuned{}
			err := testclient.Client.Get(context.TODO(), key, tuned)
			Expect(err).ToNot(HaveOccurred(), "cannot find the Cluster Node Tuning Operator object "+key.String())
			validateTunedActiveProfile(workerRTNodes)
			execSysctlOnWorkers(workerRTNodes, sysctlMap)
		})
	})

	Context("RPS configuration", func() {
		It("Should have the correct RPS configuration", func() {
			if profile.Spec.CPU == nil || profile.Spec.CPU.Reserved != nil {
				return
			}

			expectedRPSCPUs, err := cpuset.Parse(string(*profile.Spec.CPU.Reserved))
			Expect(err).ToNot(HaveOccurred())
			ociHookPath := filepath.Join("/rootfs", machineconfig.OCIHooksConfigDir, machineconfig.OCIHooksConfig+".json")
			Expect(err).ToNot(HaveOccurred())
			for _, node := range workerRTNodes {
				// Verify the OCI RPS hook uses the correct RPS mask
				hooksConfig, err := nodes.ExecCommandOnMachineConfigDaemon(&node, []string{"cat", ociHookPath})
				Expect(err).ToNot(HaveOccurred())

				var hooks map[string]interface{}
				err = json.Unmarshal(hooksConfig, &hooks)
				Expect(err).ToNot(HaveOccurred())
				hook := hooks["hook"].(map[string]interface{})
				Expect(hook).ToNot(BeNil())
				args := hook["args"].([]interface{})
				Expect(len(args)).To(Equal(2), "unexpected arguments: %v", args)

				rpsCPUs, err := components.CPUMaskToCPUSet(args[1].(string))
				Expect(err).ToNot(HaveOccurred())
				Expect(rpsCPUs).To(Equal(expectedRPSCPUs), "the hook rps mask is different from the reserved CPUs")

				// Verify the systemd RPS service uses the correct RPS mask
				cmd := []string{"sed", "-n", "s/^ExecStart=.*echo \\([A-Fa-f0-9]*\\) .*/\\1/p", "/rootfs/etc/systemd/system/update-rps@.service"}
				serviceRPSCPUs, err := nodes.ExecCommandOnNode(cmd, &node)
				Expect(err).ToNot(HaveOccurred())

				rpsCPUs, err = components.CPUMaskToCPUSet(serviceRPSCPUs)
				Expect(err).ToNot(HaveOccurred())
				Expect(rpsCPUs).To(Equal(expectedRPSCPUs), "the service rps mask is different from the reserved CPUs")

				// Verify all host network devices have the correct RPS mask
				cmd = []string{"find", "/rootfs/sys/devices", "-type", "f", "-name", "rps_cpus", "-exec", "cat", "{}", ";"}
				devsRPS, err := nodes.ExecCommandOnNode(cmd, &node)
				Expect(err).ToNot(HaveOccurred())

				for _, devRPS := range strings.Split(devsRPS, "\n") {
					rpsCPUs, err = components.CPUMaskToCPUSet(devRPS)
					Expect(err).ToNot(HaveOccurred())
					Expect(rpsCPUs).To(Equal(expectedRPSCPUs), "a host device rps mask is different from the reserved CPUs")
				}

				// Verify all node pod network devices have the correct RPS mask
				nodePods := &corev1.PodList{}
				listOptions := &client.ListOptions{
					Namespace:     "",
					FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": node.Name}),
				}
				err = testclient.Client.List(context.TODO(), nodePods, listOptions)
				Expect(err).ToNot(HaveOccurred())

				for _, pod := range nodePods.Items {
					cmd := []string{"find", "/sys/devices", "-type", "f", "-name", "rps_cpus", "-exec", "cat", "{}", ";"}
					devsRPS, err := pods.ExecCommandOnPod(testclient.K8sClient, &pod, cmd)
					for _, devRPS := range strings.Split(strings.Trim(string(devsRPS), "\n"), "\n") {
						rpsCPUs, err = components.CPUMaskToCPUSet(devRPS)
						Expect(err).ToNot(HaveOccurred())
						Expect(rpsCPUs).To(Equal(expectedRPSCPUs), pod.Name+" has a device rps mask different from the reserved CPUs")
					}
				}
			}
		})
	})

	Context("Network latency parameters adjusted by the Node Tuning Operator", func() {
		It("[test_id:28467][crit:high][vendor:cnf-qe@redhat.com][level:acceptance] Should contain configuration injected through the openshift-node-performance profile", func() {
			sysctlMap := map[string]string{
				"net.ipv4.tcp_fastopen":           "3",
				"kernel.sched_min_granularity_ns": "10000000",
				"vm.dirty_ratio":                  "10",
				"vm.dirty_background_ratio":       "3",
				"vm.swappiness":                   "10",
				"kernel.sched_migration_cost_ns":  "5000000",
			}
			key := types.NamespacedName{
				Name:      components.GetComponentName(testutils.PerformanceProfileName, components.ProfileNamePerformance),
				Namespace: components.NamespaceNodeTuningOperator,
			}
			tuned := &tunedv1.Tuned{}
			err := testclient.Client.Get(context.TODO(), key, tuned)
			Expect(err).ToNot(HaveOccurred(), "cannot find the Cluster Node Tuning Operator object "+components.ProfileNamePerformance)
			validateTunedActiveProfile(workerRTNodes)
			execSysctlOnWorkers(workerRTNodes, sysctlMap)
		})
	})

	Context("Network device queues adjusted by Tuned", func() {
		It("[test_id:40308][crit:high][vendor:cnf-qe@redhat.com][level:acceptance] Should be set to the profile's reserved CPUs count ", func() {
			noDeviceFound := true
			if profile.Spec.Net != nil {
				reservedSet, err := cpuset.Parse(string(*profile.Spec.CPU.Reserved))
				Expect(err).ToNot(HaveOccurred())
				reserveCPUsCount := reservedSet.Size()
				if profile.Spec.Net.UserLevelNetworking != nil && *profile.Spec.Net.UserLevelNetworking && len(profile.Spec.Net.Devices) == 0 {
					By("To all non virtual network devices when no devices are specified under profile.Spec.Net.Devices")
					for _, node := range workerRTNodes {

						cmdGetPhysicalDevices := []string{"find", "/sys/class/net", "-type", "l", "-not", "-lname", "*virtual*", "-printf", "%f "}
						By(fmt.Sprintf("getting a list of physical network devices: %v", cmdGetPhysicalDevices))
						tuned := tunedForNode(&node)
						phyDevs, err := pods.ExecCommandOnPod(testclient.K8sClient, tuned, cmdGetPhysicalDevices)
						Expect(err).ToNot(HaveOccurred())

						for _, d := range strings.Split(string(phyDevs), " ") {
							if d == "" {
								continue
							}
							// See if the device 'd' supports querying the channels.
							_, err := pods.ExecCommandOnPod(testclient.K8sClient, tuned, []string{"ethtool", "-l", d})
							if err == nil {
								cmdCombinedChannelsCurrent := []string{"bash", "-c",
									fmt.Sprintf("ethtool -l %s | sed -n '/Current hardware settings:/,/Combined:/{s/^Combined:\\s*//p}'", d)}

								By(fmt.Sprintf("using physical network device %s for testing", d))
								out, err := pods.ExecCommandOnPod(testclient.K8sClient, tuned, cmdCombinedChannelsCurrent)
								Expect(err).NotTo(HaveOccurred())
								channelCurrentCombined, err := strconv.Atoi(strings.TrimSpace(string(out)))
								if err != nil {
									testlog.Warningf(fmt.Sprintf("unable to retrieve current multi-purpose channels hardware settings for device %s on %s; skipping test: %v",
										d, node.Name, err))
								} else {
									Expect(err).ToNot(HaveOccurred())
									Expect(channelCurrentCombined).To(Equal(reserveCPUsCount), " Channel current combine count does not match the count of reserverd CPUs")
								}
								noDeviceFound = false
							}
						}

						if noDeviceFound {
							Skip(fmt.Sprintf("no network devices supporting querying channels found on node %s; skipping test", node.Name))
						}
					}
				}
			}
		})
	})

	Context("Create second performance profiles on a cluster", func() {
		var secondMCP *mcov1.MachineConfigPool
		var secondProfile *performancev2.PerformanceProfile
		var newRole = "worker-new"

		BeforeEach(func() {
			newLabel := fmt.Sprintf("%s/%s", testutils.LabelRole, newRole)

			reserved := performancev2.CPUSet("0")
			isolated := performancev2.CPUSet("1-3")

			secondProfile = &performancev2.PerformanceProfile{
				TypeMeta: metav1.TypeMeta{
					Kind:       "PerformanceProfile",
					APIVersion: performancev2.GroupVersion.String(),
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "second-profile",
				},
				Spec: performancev2.PerformanceProfileSpec{
					CPU: &performancev2.CPU{
						Reserved: &reserved,
						Isolated: &isolated,
					},
					NodeSelector: map[string]string{newLabel: ""},
					RealTimeKernel: &performancev2.RealTimeKernel{
						Enabled: pointer.BoolPtr(true),
					},
					AdditionalKernelArgs: []string{
						"NEW_ARGUMENT",
					},
					NUMA: &performancev2.NUMA{
						TopologyPolicy: pointer.StringPtr("restricted"),
					},
				},
			}

			machineConfigSelector := componentprofile.GetMachineConfigLabel(secondProfile)
			secondMCP = &mcov1.MachineConfigPool{
				ObjectMeta: metav1.ObjectMeta{
					Name: "second-mcp",
					Labels: map[string]string{
						machineconfigv1.MachineConfigRoleLabelKey: newRole,
					},
				},
				Spec: mcov1.MachineConfigPoolSpec{
					MachineConfigSelector: &metav1.LabelSelector{
						MatchLabels: machineConfigSelector,
					},
					NodeSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							newLabel: "",
						},
					},
				},
			}

			Expect(testclient.Client.Create(context.TODO(), secondMCP)).ToNot(HaveOccurred())
		})

		AfterEach(func() {
			if secondProfile != nil {
				if err := profiles.Delete(secondProfile.Name); err != nil {
					klog.Warningf("failed to delete the performance profile %q: %v", secondProfile.Name, err)
				}
			}

			if secondMCP != nil {
				if err := mcps.Delete(secondMCP.Name); err != nil {
					klog.Warningf("failed to delete the machine config pool %q: %v", secondMCP.Name, err)
				}
			}
		})

		It("[test_id:32364] Verifies that cluster can have multiple profiles", func() {
			Expect(testclient.Client.Create(context.TODO(), secondProfile)).ToNot(HaveOccurred())

			By("Checking that new KubeletConfig, MachineConfig and RuntimeClass created")
			configKey := types.NamespacedName{
				Name:      components.GetComponentName(secondProfile.Name, components.ComponentNamePrefix),
				Namespace: metav1.NamespaceNone,
			}
			kubeletConfig := &machineconfigv1.KubeletConfig{}
			err := testclient.GetWithRetry(context.TODO(), configKey, kubeletConfig)
			Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("cannot find KubeletConfig object %s", configKey.Name))
			Expect(kubeletConfig.Spec.MachineConfigPoolSelector.MatchLabels[machineconfigv1.MachineConfigRoleLabelKey]).Should(Equal(newRole))
			Expect(kubeletConfig.Spec.KubeletConfig.Raw).Should(ContainSubstring("restricted"), "Can't find value in KubeletConfig")

			runtimeClass := &v1beta1.RuntimeClass{}
			err = testclient.GetWithRetry(context.TODO(), configKey, runtimeClass)
			Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("cannot find RuntimeClass profile object %s", runtimeClass.Name))
			Expect(runtimeClass.Handler).Should(Equal(machineconfig.HighPerformanceRuntime))

			machineConfigKey := types.NamespacedName{
				Name:      machineconfig.GetMachineConfigName(secondProfile),
				Namespace: metav1.NamespaceNone,
			}
			machineConfig := &machineconfigv1.MachineConfig{}
			err = testclient.GetWithRetry(context.TODO(), machineConfigKey, machineConfig)
			Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("cannot find MachineConfig object %s", configKey.Name))
			Expect(machineConfig.Labels[machineconfigv1.MachineConfigRoleLabelKey]).Should(Equal(newRole))

			By("Checking that new Tuned profile created")
			tunedKey := types.NamespacedName{
				Name:      components.GetComponentName(secondProfile.Name, components.ProfileNamePerformance),
				Namespace: components.NamespaceNodeTuningOperator,
			}
			tunedProfile := &tunedv1.Tuned{}
			err = testclient.GetWithRetry(context.TODO(), tunedKey, tunedProfile)
			Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("cannot find Tuned profile object %s", tunedKey.Name))
			Expect(tunedProfile.Spec.Recommend[0].MachineConfigLabels[machineconfigv1.MachineConfigRoleLabelKey]).Should(Equal(newRole))
			Expect(*tunedProfile.Spec.Profile[0].Data).Should(ContainSubstring("NEW_ARGUMENT"), "Can't find value in Tuned profile")

			By("Checking that the initial MCP does not start updating")
			Consistently(func() corev1.ConditionStatus {
				return mcps.GetConditionStatus(testutils.RoleWorkerCNF, machineconfigv1.MachineConfigPoolUpdating)
			}, 30, 5).Should(Equal(corev1.ConditionFalse))

			By("Remove second profile and verify that KubeletConfig and MachineConfig were removed")
			Expect(testclient.Client.Delete(context.TODO(), secondProfile)).ToNot(HaveOccurred())

			profileKey := types.NamespacedName{
				Name:      secondProfile.Name,
				Namespace: secondProfile.Namespace,
			}
			Expect(profiles.WaitForDeletion(profileKey, 60*time.Second)).ToNot(HaveOccurred())

			Consistently(func() corev1.ConditionStatus {
				return mcps.GetConditionStatus(testutils.RoleWorkerCNF, machineconfigv1.MachineConfigPoolUpdating)
			}, 30, 5).Should(Equal(corev1.ConditionFalse))

			Expect(testclient.Client.Get(context.TODO(), configKey, kubeletConfig)).To(HaveOccurred(), fmt.Sprintf("KubeletConfig %s should be removed", configKey.Name))
			Expect(testclient.Client.Get(context.TODO(), machineConfigKey, machineConfig)).To(HaveOccurred(), fmt.Sprintf("MachineConfig %s should be removed", configKey.Name))
			Expect(testclient.Client.Get(context.TODO(), configKey, runtimeClass)).To(HaveOccurred(), fmt.Sprintf("RuntimeClass %s should be removed", configKey.Name))
			Expect(testclient.Client.Get(context.TODO(), tunedKey, tunedProfile)).To(HaveOccurred(), fmt.Sprintf("Tuned profile object %s should be removed", tunedKey.Name))

			By("Checking that initial KubeletConfig and MachineConfig still exist")
			initialKey := types.NamespacedName{
				Name:      components.GetComponentName(profile.Name, components.ComponentNamePrefix),
				Namespace: components.NamespaceNodeTuningOperator,
			}
			err = testclient.GetWithRetry(context.TODO(), initialKey, &machineconfigv1.KubeletConfig{})
			Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("cannot find KubeletConfig object %s", initialKey.Name))

			initialMachineConfigKey := types.NamespacedName{
				Name:      machineconfig.GetMachineConfigName(profile),
				Namespace: metav1.NamespaceNone,
			}
			err = testclient.GetWithRetry(context.TODO(), initialMachineConfigKey, &machineconfigv1.MachineConfig{})
			Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("cannot find MachineConfig object %s", initialKey.Name))
		})
	})

	Context("Verify API Conversions", func() {
		verifyV2V1 := func() {
			By("Checking v2 -> v1 conversion")
			v1Profile := &performancev1.PerformanceProfile{}
			key := types.NamespacedName{
				Name:      profile.Name,
				Namespace: profile.Namespace,
			}

			err := testclient.Client.Get(context.TODO(), key, v1Profile)
			Expect(err).ToNot(HaveOccurred(), "Failed getting v1Profile")
			Expect(verifyV2Conversion(profile, v1Profile)).ToNot(HaveOccurred())

			By("Checking v1 -> v2 conversion")
			v1Profile.Name = "v1"
			v1Profile.ResourceVersion = ""
			v1Profile.Spec.NodeSelector = map[string]string{"v1/v1": "v1"}
			Expect(testclient.Client.Create(context.TODO(), v1Profile)).ToNot(HaveOccurred())

			key = types.NamespacedName{
				Name:      v1Profile.Name,
				Namespace: v1Profile.Namespace,
			}

			defer func() {
				Expect(testclient.Client.Delete(context.TODO(), v1Profile)).ToNot(HaveOccurred())
				Expect(profiles.WaitForDeletion(key, 60*time.Second)).ToNot(HaveOccurred())
			}()

			v2Profile := &performancev2.PerformanceProfile{}
			err = testclient.GetWithRetry(context.TODO(), key, v2Profile)
			Expect(err).ToNot(HaveOccurred(), "Failed getting v2Profile")
			Expect(verifyV2Conversion(v2Profile, v1Profile)).ToNot(HaveOccurred())
		}

		verifyV1VAlpha1 := func() {
			By("Acquiring the tests profile as a v1 profile")
			v1Profile := &performancev1.PerformanceProfile{}
			key := types.NamespacedName{
				Name:      profile.Name,
				Namespace: profile.Namespace,
			}

			err := testclient.Client.Get(context.TODO(), key, v1Profile)
			Expect(err).ToNot(HaveOccurred(), "Failed acquiring a v1 profile")

			By("Checking v1 -> v1alpha1 conversion")
			v1alpha1Profile := &performancev1alpha1.PerformanceProfile{}
			key = types.NamespacedName{
				Name:      v1Profile.Name,
				Namespace: v1Profile.Namespace,
			}

			err = testclient.Client.Get(context.TODO(), key, v1alpha1Profile)
			Expect(err).ToNot(HaveOccurred(), "Failed getting v1alpha1Profile")
			Expect(verifyV1alpha1Conversion(v1alpha1Profile, v1Profile)).ToNot(HaveOccurred())

			By("Checking v1alpha1 -> v1 conversion")
			v1alpha1Profile.Name = "v1alpha"
			v1alpha1Profile.ResourceVersion = ""
			v1alpha1Profile.Spec.NodeSelector = map[string]string{"v1alpha/v1alpha": "v1alpha"}
			Expect(testclient.Client.Create(context.TODO(), v1alpha1Profile)).ToNot(HaveOccurred())

			key = types.NamespacedName{
				Name:      v1alpha1Profile.Name,
				Namespace: v1alpha1Profile.Namespace,
			}

			defer func() {
				Expect(testclient.Client.Delete(context.TODO(), v1alpha1Profile)).ToNot(HaveOccurred())
				Expect(profiles.WaitForDeletion(key, 60*time.Second)).ToNot(HaveOccurred())
			}()

			v1Profile = &performancev1.PerformanceProfile{}
			err = testclient.GetWithRetry(context.TODO(), key, v1Profile)
			Expect(err).ToNot(HaveOccurred(), "Failed getting v1profile")
			Expect(verifyV1alpha1Conversion(v1alpha1Profile, v1Profile)).ToNot(HaveOccurred())
		}

		When("the performance profile does not contain NUMA field", func() {
			BeforeEach(func() {
				key := types.NamespacedName{
					Name:      profile.Name,
					Namespace: profile.Namespace,
				}
				err := testclient.Client.Get(context.TODO(), key, profile)
				Expect(err).ToNot(HaveOccurred(), "Failed getting v1Profile")

				profile.Name = "without-numa"
				profile.ResourceVersion = ""
				profile.Spec.NodeSelector = map[string]string{"withoutNUMA/withoutNUMA": "withoutNUMA"}
				profile.Spec.NUMA = nil

				err = testclient.Client.Create(context.TODO(), profile)
				Expect(err).ToNot(HaveOccurred(), "Failed to create profile without NUMA")
			})

			AfterEach(func() {
				Expect(testclient.Client.Delete(context.TODO(), profile)).ToNot(HaveOccurred())
				Expect(profiles.WaitForDeletion(types.NamespacedName{
					Name:      profile.Name,
					Namespace: profile.Namespace,
				}, 60*time.Second)).ToNot(HaveOccurred())
			})

			It("Verifies v1 <-> v1alpha1 conversions", func() {
				verifyV1VAlpha1()
			})

			It("Verifies v1 <-> v2 conversions", func() {
				verifyV2V1()
			})
		})

		It("[test_id:35887] Verifies v1 <-> v1alpha1 conversions", func() {
			verifyV1VAlpha1()
		})

		It("[test_id:35888] Verifies v1 <-> v2 conversions", func() {
			verifyV2V1()
		})
	})

	Context("Validation webhook", func() {
		BeforeEach(func() {
			if discovery.Enabled() {
				Skip("Discovery mode enabled, test skipped because it creates incorrect profiles")
			}
		})

		validateObject := func(obj client.Object, message string) {
			err := testclient.Client.Create(context.TODO(), obj)
			Expect(err).To(HaveOccurred(), "expected the validation error")
			Expect(err.Error()).To(ContainSubstring(message))
		}

		Context("with API version v1alpha1 profile", func() {
			var v1alpha1Profile *performancev1alpha1.PerformanceProfile

			BeforeEach(func() {
				v1alpha1Profile = &performancev1alpha1.PerformanceProfile{
					TypeMeta: metav1.TypeMeta{
						Kind:       "PerformanceProfile",
						APIVersion: performancev1alpha1.GroupVersion.String(),
					},
					ObjectMeta: metav1.ObjectMeta{
						Name: "v1alpha1-profile",
					},
					Spec: performancev1alpha1.PerformanceProfileSpec{
						RealTimeKernel: &performancev1alpha1.RealTimeKernel{
							Enabled: pointer.BoolPtr(true),
						},
						NodeSelector: map[string]string{"v1alpha1/v1alpha1": "v1alpha1"},
						NUMA: &performancev1alpha1.NUMA{
							TopologyPolicy: pointer.StringPtr("restricted"),
						},
					},
				}
			})

			It("should reject the creation of the profile with overlapping CPUs", func() {
				reserved := performancev1alpha1.CPUSet("0-3")
				isolated := performancev1alpha1.CPUSet("0-7")

				v1alpha1Profile.Spec.CPU = &performancev1alpha1.CPU{
					Reserved: &reserved,
					Isolated: &isolated,
				}
				validateObject(v1alpha1Profile, "reserved and isolated cpus overlap")
			})

			It("should reject the creation of the profile with no isolated CPUs", func() {
				reserved := performancev1alpha1.CPUSet("0-3")
				isolated := performancev1alpha1.CPUSet("")

				v1alpha1Profile.Spec.CPU = &performancev1alpha1.CPU{
					Reserved: &reserved,
					Isolated: &isolated,
				}
				validateObject(v1alpha1Profile, "isolated CPUs can not be empty")
			})

			It("should reject the creation of the profile with the node selector that already in use", func() {
				reserved := performancev1alpha1.CPUSet("0,1")
				isolated := performancev1alpha1.CPUSet("2,3")

				v1alpha1Profile.Spec.CPU = &performancev1alpha1.CPU{
					Reserved: &reserved,
					Isolated: &isolated,
				}
				v1alpha1Profile.Spec.NodeSelector = testutils.NodeSelectorLabels
				validateObject(v1alpha1Profile, "the profile has the same node selector as the performance profile")
			})
		})

		Context("with API version v1 profile", func() {
			var v1Profile *performancev1.PerformanceProfile

			BeforeEach(func() {
				v1Profile = &performancev1.PerformanceProfile{
					TypeMeta: metav1.TypeMeta{
						Kind:       "PerformanceProfile",
						APIVersion: performancev1.GroupVersion.String(),
					},
					ObjectMeta: metav1.ObjectMeta{
						Name: "v1-profile",
					},
					Spec: performancev1.PerformanceProfileSpec{
						RealTimeKernel: &performancev1.RealTimeKernel{
							Enabled: pointer.BoolPtr(true),
						},
						NodeSelector: map[string]string{"v1/v1": "v1"},
						NUMA: &performancev1.NUMA{
							TopologyPolicy: pointer.StringPtr("restricted"),
						},
					},
				}
			})

			It("should reject the creation of the profile with overlapping CPUs", func() {
				reserved := performancev1.CPUSet("0-3")
				isolated := performancev1.CPUSet("0-7")

				v1Profile.Spec.CPU = &performancev1.CPU{
					Reserved: &reserved,
					Isolated: &isolated,
				}
				validateObject(v1Profile, "reserved and isolated cpus overlap")
			})

			It("should reject the creation of the profile with no isolated CPUs", func() {
				reserved := performancev1.CPUSet("0-3")
				isolated := performancev1.CPUSet("")

				v1Profile.Spec.CPU = &performancev1.CPU{
					Reserved: &reserved,
					Isolated: &isolated,
				}
				validateObject(v1Profile, "isolated CPUs can not be empty")
			})

			It("should reject the creation of the profile with the node selector that already in use", func() {
				reserved := performancev1.CPUSet("0,1")
				isolated := performancev1.CPUSet("2,3")

				v1Profile.Spec.CPU = &performancev1.CPU{
					Reserved: &reserved,
					Isolated: &isolated,
				}
				v1Profile.Spec.NodeSelector = testutils.NodeSelectorLabels
				validateObject(v1Profile, "the profile has the same node selector as the performance profile")
			})
		})

		Context("with profile version v2", func() {
			var v2Profile *performancev2.PerformanceProfile

			BeforeEach(func() {
				v2Profile = &performancev2.PerformanceProfile{
					TypeMeta: metav1.TypeMeta{
						Kind:       "PerformanceProfile",
						APIVersion: performancev2.GroupVersion.String(),
					},
					ObjectMeta: metav1.ObjectMeta{
						Name: "v2-profile",
					},
					Spec: performancev2.PerformanceProfileSpec{
						RealTimeKernel: &performancev2.RealTimeKernel{
							Enabled: pointer.BoolPtr(true),
						},
						NodeSelector: map[string]string{"v2/v2": "v2"},
						NUMA: &performancev2.NUMA{
							TopologyPolicy: pointer.StringPtr("restricted"),
						},
					},
				}
			})

			It("should reject the creation of the profile with overlapping CPUs", func() {
				reserved := performancev2.CPUSet("0-3")
				isolated := performancev2.CPUSet("0-7")

				v2Profile.Spec.CPU = &performancev2.CPU{
					Reserved: &reserved,
					Isolated: &isolated,
				}
				validateObject(v2Profile, "reserved and isolated cpus overlap")
			})

			It("should reject the creation of the profile with no isolated CPUs", func() {
				reserved := performancev2.CPUSet("0-3")
				isolated := performancev2.CPUSet("")

				v2Profile.Spec.CPU = &performancev2.CPU{
					Reserved: &reserved,
					Isolated: &isolated,
				}
				validateObject(v2Profile, "isolated CPUs can not be empty")
			})

			It("should reject the creation of the profile with the node selector that already in use", func() {
				reserved := performancev2.CPUSet("0,1")
				isolated := performancev2.CPUSet("2,3")

				v2Profile.Spec.CPU = &performancev2.CPU{
					Reserved: &reserved,
					Isolated: &isolated,
				}
				v2Profile.Spec.NodeSelector = testutils.NodeSelectorLabels
				validateObject(v2Profile, "the profile has the same node selector as the performance profile")
			})
		})
	})
})

func verifyV1alpha1Conversion(v1alpha1Profile *performancev1alpha1.PerformanceProfile, v1Profile *performancev1.PerformanceProfile) error {
	specCPU := v1alpha1Profile.Spec.CPU
	if (specCPU == nil) != (v1Profile.Spec.CPU == nil) {
		return fmt.Errorf("spec CPUs field is different")
	}

	if specCPU != nil {
		if (specCPU.Reserved == nil) != (v1Profile.Spec.CPU.Reserved == nil) {
			return fmt.Errorf("spec CPUs Reserved field is different")
		}
		if specCPU.Reserved != nil {
			if string(*specCPU.Reserved) != string(*v1Profile.Spec.CPU.Reserved) {
				return fmt.Errorf("reserved CPUs are different [v1alpha1: %s, v1: %s]",
					*specCPU.Reserved, *v1Profile.Spec.CPU.Reserved)
			}
		}

		if (specCPU.Isolated == nil) != (v1Profile.Spec.CPU.Isolated == nil) {
			return fmt.Errorf("spec CPUs Isolated field is different")
		}
		if specCPU.Isolated != nil {
			if string(*specCPU.Isolated) != string(*v1Profile.Spec.CPU.Isolated) {
				return fmt.Errorf("isolated CPUs are different [v1alpha1: %s, v1: %s]",
					*specCPU.Isolated, *v1Profile.Spec.CPU.Isolated)
			}
		}

		if (specCPU.BalanceIsolated == nil) != (v1Profile.Spec.CPU.BalanceIsolated == nil) {
			return fmt.Errorf("spec CPUs BalanceIsolated field is different")
		}
		if specCPU.BalanceIsolated != nil {
			if *specCPU.BalanceIsolated != *v1Profile.Spec.CPU.BalanceIsolated {
				return fmt.Errorf("balanceIsolated field is different [v1alpha1: %t, v1: %t]",
					*specCPU.BalanceIsolated, *v1Profile.Spec.CPU.BalanceIsolated)
			}
		}
	}

	specHugePages := v1alpha1Profile.Spec.HugePages
	if (specHugePages == nil) != (v1Profile.Spec.HugePages == nil) {
		return fmt.Errorf("spec HugePages field is different")
	}

	if specHugePages != nil {
		if (specHugePages.DefaultHugePagesSize == nil) != (v1Profile.Spec.HugePages.DefaultHugePagesSize == nil) {
			return fmt.Errorf("spec HugePages defaultHugePagesSize field is different")
		}
		if specHugePages.DefaultHugePagesSize != nil {
			if string(*specHugePages.DefaultHugePagesSize) != string(*v1Profile.Spec.HugePages.DefaultHugePagesSize) {
				return fmt.Errorf("defaultHugePagesSize field is different [v1alpha1: %s, v1: %s]",
					*specHugePages.DefaultHugePagesSize, *v1Profile.Spec.HugePages.DefaultHugePagesSize)
			}
		}

		if len(specHugePages.Pages) != len(v1Profile.Spec.HugePages.Pages) {
			return fmt.Errorf("pages field is different [v1alpha1: %v, v1: %v]",
				specHugePages.Pages, v1Profile.Spec.HugePages.Pages)
		}

		for i, v1alpha1Page := range specHugePages.Pages {
			v1page := v1Profile.Spec.HugePages.Pages[i]
			if string(v1alpha1Page.Size) != string(v1page.Size) ||
				(v1alpha1Page.Node == nil) != (v1page.Node == nil) ||
				(v1alpha1Page.Node != nil && *v1alpha1Page.Node != *v1page.Node) ||
				v1alpha1Page.Count != v1page.Count {
				return fmt.Errorf("pages field is different [v1alpha1: %v, v1: %v]",
					specHugePages.Pages, v1Profile.Spec.HugePages.Pages)
			}
		}
	}

	if !reflect.DeepEqual(v1alpha1Profile.Spec.MachineConfigLabel, v1Profile.Spec.MachineConfigLabel) {
		return fmt.Errorf("machineConfigLabel field is different [v1alpha1: %v, v1: %v]",
			v1alpha1Profile.Spec.MachineConfigLabel, v1Profile.Spec.MachineConfigLabel)
	}

	if !reflect.DeepEqual(v1alpha1Profile.Spec.MachineConfigPoolSelector, v1Profile.Spec.MachineConfigPoolSelector) {
		return fmt.Errorf("machineConfigPoolSelector field is different [v1alpha1: %v, v1: %v]",
			v1alpha1Profile.Spec.MachineConfigPoolSelector, v1Profile.Spec.MachineConfigPoolSelector)
	}

	if !reflect.DeepEqual(v1alpha1Profile.Spec.NodeSelector, v1Profile.Spec.NodeSelector) {
		return fmt.Errorf("nodeSelector field is different [v1alpha1: %v, v1: %v]",
			v1alpha1Profile.Spec.NodeSelector, v1Profile.Spec.NodeSelector)
	}

	specRealTimeKernel := v1alpha1Profile.Spec.RealTimeKernel
	if (specRealTimeKernel == nil) != (v1Profile.Spec.RealTimeKernel == nil) {
		return fmt.Errorf("spec RealTimeKernel field is different")
	}

	if specRealTimeKernel != nil {
		if (specRealTimeKernel.Enabled == nil) != (v1Profile.Spec.RealTimeKernel.Enabled == nil) {
			return fmt.Errorf("spec RealTimeKernel.Enabled field is different")
		}

		if specRealTimeKernel.Enabled != nil {
			if *specRealTimeKernel.Enabled != *v1Profile.Spec.RealTimeKernel.Enabled {
				return fmt.Errorf("specRealTimeKernel field is different [v1alpha1: %t, v1: %t]",
					*specRealTimeKernel.Enabled, *v1Profile.Spec.RealTimeKernel.Enabled)
			}
		}
	}

	if !reflect.DeepEqual(v1alpha1Profile.Spec.AdditionalKernelArgs, v1Profile.Spec.AdditionalKernelArgs) {
		return fmt.Errorf("additionalKernelArgs field is different [v1alpha1: %v, v1: %v]",
			v1alpha1Profile.Spec.AdditionalKernelArgs, v1Profile.Spec.AdditionalKernelArgs)
	}

	specNUMA := v1alpha1Profile.Spec.NUMA
	if (specNUMA == nil) != (v1Profile.Spec.NUMA == nil) {
		return fmt.Errorf("spec NUMA field is different")
	}

	if specNUMA != nil {
		if (specNUMA.TopologyPolicy == nil) != (v1Profile.Spec.NUMA.TopologyPolicy == nil) {
			return fmt.Errorf("spec NUMA topologyPolicy field is different")
		}
		if specNUMA.TopologyPolicy != nil {
			if *specNUMA.TopologyPolicy != *v1Profile.Spec.NUMA.TopologyPolicy {
				return fmt.Errorf("topologyPolicy field is different [v1alpha1: %s, v1: %s]",
					*specNUMA.TopologyPolicy, *v1Profile.Spec.NUMA.TopologyPolicy)
			}
		}
	}

	return nil
}

func verifyV2Conversion(v2Profile *performancev2.PerformanceProfile, v1Profile *performancev1.PerformanceProfile) error {
	specCPU := v2Profile.Spec.CPU
	if (specCPU == nil) != (v1Profile.Spec.CPU == nil) {
		return fmt.Errorf("spec CPUs field is different")
	}

	if specCPU != nil {
		if (specCPU.Reserved == nil) != (v1Profile.Spec.CPU.Reserved == nil) {
			return fmt.Errorf("spec CPUs Reserved field is different")
		}
		if specCPU.Reserved != nil {
			if string(*specCPU.Reserved) != string(*v1Profile.Spec.CPU.Reserved) {
				return fmt.Errorf("reserved CPUs are different [v2: %s, v1: %s]",
					*specCPU.Reserved, *v1Profile.Spec.CPU.Reserved)
			}
		}

		if (specCPU.Isolated == nil) != (v1Profile.Spec.CPU.Isolated == nil) {
			return fmt.Errorf("spec CPUs Isolated field is different")
		}
		if specCPU.Isolated != nil {
			if string(*specCPU.Isolated) != string(*v1Profile.Spec.CPU.Isolated) {
				return fmt.Errorf("isolated CPUs are different [v2: %s, v1: %s]",
					*specCPU.Isolated, *v1Profile.Spec.CPU.Isolated)
			}
		}

		if (specCPU.BalanceIsolated == nil) != (v1Profile.Spec.CPU.BalanceIsolated == nil) {
			return fmt.Errorf("spec CPUs BalanceIsolated field is different")
		}
		if specCPU.BalanceIsolated != nil {
			if *specCPU.BalanceIsolated != *v1Profile.Spec.CPU.BalanceIsolated {
				return fmt.Errorf("balanceIsolated field is different [v2: %t, v1: %t]",
					*specCPU.BalanceIsolated, *v1Profile.Spec.CPU.BalanceIsolated)
			}
		}
	}

	specHugePages := v2Profile.Spec.HugePages
	if (specHugePages == nil) != (v1Profile.Spec.HugePages == nil) {
		return fmt.Errorf("spec HugePages field is different")
	}

	if specHugePages != nil {
		if (specHugePages.DefaultHugePagesSize == nil) != (v1Profile.Spec.HugePages.DefaultHugePagesSize == nil) {
			return fmt.Errorf("spec HugePages defaultHugePagesSize field is different")
		}
		if specHugePages.DefaultHugePagesSize != nil {
			if string(*specHugePages.DefaultHugePagesSize) != string(*v1Profile.Spec.HugePages.DefaultHugePagesSize) {
				return fmt.Errorf("defaultHugePagesSize field is different [v2: %s, v1: %s]",
					*specHugePages.DefaultHugePagesSize, *v1Profile.Spec.HugePages.DefaultHugePagesSize)
			}
		}

		if len(specHugePages.Pages) != len(v1Profile.Spec.HugePages.Pages) {
			return fmt.Errorf("pages field is different [v2: %v, v1: %v]",
				specHugePages.Pages, v1Profile.Spec.HugePages.Pages)
		}

		for i, v1alpha1Page := range specHugePages.Pages {
			v1page := v1Profile.Spec.HugePages.Pages[i]
			if string(v1alpha1Page.Size) != string(v1page.Size) ||
				(v1alpha1Page.Node == nil) != (v1page.Node == nil) ||
				(v1alpha1Page.Node != nil && *v1alpha1Page.Node != *v1page.Node) ||
				v1alpha1Page.Count != v1page.Count {
				return fmt.Errorf("pages field is different [v2: %v, v1: %v]",
					specHugePages.Pages, v1Profile.Spec.HugePages.Pages)
			}
		}
	}

	if !reflect.DeepEqual(v2Profile.Spec.MachineConfigLabel, v1Profile.Spec.MachineConfigLabel) {
		return fmt.Errorf("machineConfigLabel field is different [v2: %v, v1: %v]",
			v2Profile.Spec.MachineConfigLabel, v1Profile.Spec.MachineConfigLabel)
	}

	if !reflect.DeepEqual(v2Profile.Spec.MachineConfigPoolSelector, v1Profile.Spec.MachineConfigPoolSelector) {
		return fmt.Errorf("machineConfigPoolSelector field is different [v2: %v, v1: %v]",
			v2Profile.Spec.MachineConfigPoolSelector, v1Profile.Spec.MachineConfigPoolSelector)
	}

	if !reflect.DeepEqual(v2Profile.Spec.NodeSelector, v1Profile.Spec.NodeSelector) {
		return fmt.Errorf("nodeSelector field is different [v2: %v, v1: %v]",
			v2Profile.Spec.NodeSelector, v1Profile.Spec.NodeSelector)
	}

	specRealTimeKernel := v2Profile.Spec.RealTimeKernel
	if (specRealTimeKernel == nil) != (v1Profile.Spec.RealTimeKernel == nil) {
		return fmt.Errorf("spec RealTimeKernel field is different")
	}

	if specRealTimeKernel != nil {
		if (specRealTimeKernel.Enabled == nil) != (v1Profile.Spec.RealTimeKernel.Enabled == nil) {
			return fmt.Errorf("spec RealTimeKernel.Enabled field is different")
		}

		if specRealTimeKernel.Enabled != nil {
			if *specRealTimeKernel.Enabled != *v1Profile.Spec.RealTimeKernel.Enabled {
				return fmt.Errorf("specRealTimeKernel field is different [v2: %t, v1: %t]",
					*specRealTimeKernel.Enabled, *v1Profile.Spec.RealTimeKernel.Enabled)
			}
		}
	}

	if !reflect.DeepEqual(v2Profile.Spec.AdditionalKernelArgs, v1Profile.Spec.AdditionalKernelArgs) {
		return fmt.Errorf("additionalKernelArgs field is different [v2: %v, v1: %v]",
			v2Profile.Spec.AdditionalKernelArgs, v1Profile.Spec.AdditionalKernelArgs)
	}

	specNUMA := v2Profile.Spec.NUMA
	if (specNUMA == nil) != (v1Profile.Spec.NUMA == nil) {
		return fmt.Errorf("spec NUMA field is different")
	}

	if specNUMA != nil {
		if (specNUMA.TopologyPolicy == nil) != (v1Profile.Spec.NUMA.TopologyPolicy == nil) {
			return fmt.Errorf("spec NUMA topologyPolicy field is different")
		}
		if specNUMA.TopologyPolicy != nil {
			if *specNUMA.TopologyPolicy != *v1Profile.Spec.NUMA.TopologyPolicy {
				return fmt.Errorf("topologyPolicy field is different [v2: %s, v1: %s]",
					*specNUMA.TopologyPolicy, *v1Profile.Spec.NUMA.TopologyPolicy)
			}
		}
	}

	for _, f := range v2Profile.GetObjectMeta().GetManagedFields() {
		if f.APIVersion == performancev1alpha1.GroupVersion.String() ||
			f.APIVersion == performancev1.GroupVersion.String() {
			if v2Profile.Spec.GloballyDisableIrqLoadBalancing == nil || !*v2Profile.Spec.GloballyDisableIrqLoadBalancing {
				return fmt.Errorf("globallyDisableIrqLoadBalancing field must be set to true")
			}
		}
	}

	return nil
}

func execSysctlOnWorkers(workerNodes []corev1.Node, sysctlMap map[string]string) {
	var err error
	var out []byte
	for _, node := range workerNodes {
		for param, expected := range sysctlMap {
			By(fmt.Sprintf("executing the command \"sysctl -n %s\"", param))
			out, err = nodes.ExecCommandOnMachineConfigDaemon(&node, []string{"sysctl", "-n", param})
			Expect(err).ToNot(HaveOccurred())
			Expect(strings.TrimSpace(string(out))).Should(Equal(expected), "parameter %s value is not %s.", param, expected)
		}
	}
}

// execute sysctl command inside container in a tuned pod
func validateTunedActiveProfile(nodes []corev1.Node) {
	var err error
	var out []byte
	activeProfileName := components.GetComponentName(testutils.PerformanceProfileName, components.ProfileNamePerformance)

	// check if some another Tuned profile overwrites PAO profile
	tunedList := &tunedv1.TunedList{}
	err = testclient.Client.List(context.TODO(), tunedList)
	Expect(err).NotTo(HaveOccurred())

	for _, t := range tunedList.Items {
		if len(t.Spec.Profile) > 0 && t.Spec.Profile[0].Data != nil && strings.Contains(*t.Spec.Profile[0].Data, fmt.Sprintf("include=%s", activeProfileName)) {
			testlog.Warning(fmt.Sprintf("PAO tuned profile amended by '%s' profile, test may fail", t.Name))
			if t.Spec.Profile[0].Name != nil {
				activeProfileName = *t.Spec.Profile[0].Name
			}
		}
	}

	for _, node := range nodes {
		tuned := tunedForNode(&node)
		tunedName := tuned.ObjectMeta.Name
		By(fmt.Sprintf("executing the command cat /etc/tuned/active_profile inside the pod %s", tunedName))
		Eventually(func() string {
			out, err = pods.ExecCommandOnPod(testclient.K8sClient, tuned, []string{"cat", "/etc/tuned/active_profile"})
			return strings.TrimSpace(string(out))
		}, cluster.ComputeTestTimeout(testTimeout*time.Second, RunningOnSingleNode), testPollInterval*time.Second).Should(Equal(activeProfileName),
			fmt.Sprintf("active_profile is not set to %s. %v", activeProfileName, err))
	}
}

// find tuned pod for appropriate node
func tunedForNode(node *corev1.Node) *corev1.Pod {
	listOptions := &client.ListOptions{
		Namespace:     components.NamespaceNodeTuningOperator,
		FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": node.Name}),
		LabelSelector: labels.SelectorFromSet(labels.Set{"openshift-app": "tuned"}),
	}

	tunedList := &corev1.PodList{}
	Eventually(func() bool {
		if err := testclient.Client.List(context.TODO(), tunedList, listOptions); err != nil {
			return false
		}

		if len(tunedList.Items) == 0 {
			return false
		}
		for _, s := range tunedList.Items[0].Status.ContainerStatuses {
			if s.Ready == false {
				return false
			}
		}
		return true

	}, cluster.ComputeTestTimeout(testTimeout*time.Second, RunningOnSingleNode), testPollInterval*time.Second).Should(BeTrue(),
		"there should be one tuned daemon per node")

	return &tunedList.Items[0]
}
