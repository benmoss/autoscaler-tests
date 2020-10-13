/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package autoscaling

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo"
	v1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2erc "k8s.io/kubernetes/test/e2e/framework/rc"
	"k8s.io/kubernetes/test/e2e/scheduling"
	testutils "k8s.io/kubernetes/test/utils"
	imageutils "k8s.io/kubernetes/test/utils/image"
)

const (
	defaultTimeout         = 3 * time.Minute
	manualResizeTimeout    = 6 * time.Minute
	scaleUpTimeout         = 5 * time.Minute
	scaleUpTriggerTimeout  = 2 * time.Minute
	scaleDownTimeout       = 20 * time.Minute
	podTimeout             = 2 * time.Minute
	rcCreationRetryTimeout = 4 * time.Minute
	rcCreationRetryDelay   = 20 * time.Second
	makeSchedulableTimeout = 10 * time.Minute
	makeSchedulableDelay   = 20 * time.Second
	freshStatusLimit       = 20 * time.Second

	disabledTaint             = "DisabledForAutoscalingTest"
	criticalAddonsOnlyTaint   = "CriticalAddonsOnly"
	newNodesForScaledownTests = 2

	caNoScaleUpStatus      = "NoActivity"
	caOngoingScaleUpStatus = "InProgress"
	timestampFormat        = "2006-01-02 15:04:05 -0700 MST"

	expendablePriorityClassName = "expendable-priority"
	highPriorityClassName       = "high-priority"
)

var _ = ginkgo.Describe("Cluster size autoscaling [Slow]", func() {
	f := framework.NewDefaultFramework("autoscaling")
	var c clientset.Interface
	var nodeCount int
	var memAllocatableMb int
	var originalSizes map[string]int
	var provider Provider

	ginkgo.BeforeEach(func() {
		c = f.ClientSet
		var ok bool
		provider, ok = framework.TestContext.CloudConfig.Provider.(Provider)
		if !ok {
			ginkgo.Skip("provider does not support autoscaler tests")
		}

		originalSizes = make(map[string]int)
		sum := 0
		for _, mig := range strings.Split(framework.TestContext.CloudConfig.NodeInstanceGroup, ",") {
			size, err := framework.GroupSize(mig)
			framework.ExpectNoError(err)
			ginkgo.By(fmt.Sprintf("Initial size of %s: %d", mig, size))
			originalSizes[mig] = size
			sum += size
		}
		// Give instances time to spin up
		framework.ExpectNoError(e2enode.WaitForReadyNodes(c, sum, scaleUpTimeout))

		nodes, err := e2enode.GetReadySchedulableNodes(f.ClientSet)
		framework.ExpectNoError(err)
		nodeCount = len(nodes.Items)
		ginkgo.By(fmt.Sprintf("Initial number of schedulable nodes: %v", nodeCount))
		framework.ExpectNotEqual(nodeCount, 0)
		mem := nodes.Items[0].Status.Allocatable[v1.ResourceMemory]
		memAllocatableMb = int((&mem).Value() / 1024 / 1024)
		ginkgo.By(fmt.Sprintf("Using node %q to determine that we have %dMi of memory on each node", nodes.Items[0].Name, memAllocatableMb))

		framework.ExpectEqual(nodeCount, sum)

		framework.ExpectNoError(provider.EnableAutoscaler("default-pool", 3, 5))
	})

	ginkgo.AfterEach(func() {
		framework.ExpectNoError(provider.DisableAutoscaler("default-pool"))

		ginkgo.By("Restoring initial size of the cluster")
		setMigSizes(originalSizes)
		expectedNodes := 0
		for _, size := range originalSizes {
			expectedNodes += size
		}
		framework.ExpectNoError(e2enode.WaitForReadyNodes(c, expectedNodes, scaleDownTimeout))
		nodes, err := c.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
		framework.ExpectNoError(err)

		s := time.Now()
	makeSchedulableLoop:
		for start := time.Now(); time.Since(start) < makeSchedulableTimeout; time.Sleep(makeSchedulableDelay) {
			for i := range nodes.Items {
				err = makeNodeSchedulable(c, &nodes.Items[i], true)
				switch err.(type) {
				case CriticalAddonsOnlyError:
					continue makeSchedulableLoop
				default:
					framework.ExpectNoError(err)
				}
			}
			break
		}
		klog.Infof("Made nodes schedulable again in %v", time.Since(s).String())
	})

	ginkgo.FIt("shouldn't increase cluster size if pending pod is too large [Feature:ClusterSizeAutoscalingScaleUp]", func() {
		ginkgo.By("Creating unschedulable pod")
		ReserveMemory(f, "memory-reservation", 1, int(1.1*float64(memAllocatableMb)), false, defaultTimeout)
		defer e2erc.DeleteRCAndWaitForGC(f.ClientSet, f.Namespace.Name, "memory-reservation")

		ginkgo.By("Waiting for scale up hoping it won't happen")
		// Verify that the appropriate event was generated
		eventFound := false
	EventsLoop:
		for start := time.Now(); time.Since(start) < scaleUpTimeout; time.Sleep(20 * time.Second) {
			ginkgo.By("Waiting for NotTriggerScaleUp event")
			events, err := f.ClientSet.CoreV1().Events(f.Namespace.Name).List(context.TODO(), metav1.ListOptions{})
			framework.ExpectNoError(err)

			for _, e := range events.Items {
				if e.InvolvedObject.Kind == "Pod" && e.Reason == "NotTriggerScaleUp" && strings.Contains(e.Message, "it wouldn't fit if a new node is added") {
					ginkgo.By("NotTriggerScaleUp event found")
					eventFound = true
					break EventsLoop
				}
			}
		}
		framework.ExpectEqual(eventFound, true)
		// Verify that cluster size is not changed
		framework.ExpectNoError(WaitForClusterSizeFunc(f.ClientSet,
			func(size int) bool { return size <= nodeCount }, time.Second))
	})

	simpleScaleUpTest := func(unready int) {
		ReserveMemory(f, "memory-reservation", 100, int(1.1*float64(nodeCount*memAllocatableMb)), false, 1*time.Second)
		defer e2erc.DeleteRCAndWaitForGC(f.ClientSet, f.Namespace.Name, "memory-reservation")

		// Verify that cluster size is increased
		framework.ExpectNoError(WaitForClusterSizeFuncWithUnready(f.ClientSet,
			func(size int) bool { return size >= nodeCount+1 }, scaleUpTimeout, unready))
		framework.ExpectNoError(waitForAllCaPodsReadyInNamespace(f, c))
	}

	ginkgo.It("should increase cluster size if pending pods are small [Feature:ClusterSizeAutoscalingScaleUp]",
		func() { simpleScaleUpTest(0) })

	ginkgo.It("shouldn't trigger additional scale-ups during processing scale-up [Feature:ClusterSizeAutoscalingScaleUp]", func() {
		// Wait for the situation to stabilize - CA should be running and have up-to-date node readiness info.
		status, err := waitForScaleUpStatus(c, func(s *scaleUpStatus) bool {
			return s.ready == s.target && s.ready <= nodeCount
		}, scaleUpTriggerTimeout)
		framework.ExpectNoError(err)

		unmanagedNodes := nodeCount - status.ready

		ginkgo.By("Schedule more pods than can fit and wait for cluster to scale-up")
		ReserveMemory(f, "memory-reservation", 100, nodeCount*memAllocatableMb, false, 1*time.Second)
		defer e2erc.DeleteRCAndWaitForGC(f.ClientSet, f.Namespace.Name, "memory-reservation")

		status, err = waitForScaleUpStatus(c, func(s *scaleUpStatus) bool {
			return s.status == caOngoingScaleUpStatus
		}, scaleUpTriggerTimeout)
		framework.ExpectNoError(err)
		target := status.target
		framework.ExpectNoError(waitForAllCaPodsReadyInNamespace(f, c))

		ginkgo.By("Expect no more scale-up to be happening after all pods are scheduled")

		// wait for a while until scale-up finishes; we cannot read CA status immediately
		// after pods are scheduled as status config map is updated by CA once every loop iteration
		status, err = waitForScaleUpStatus(c, func(s *scaleUpStatus) bool {
			return s.status == caNoScaleUpStatus
		}, 2*freshStatusLimit)
		framework.ExpectNoError(err)

		if status.target != target {
			klog.Warningf("Final number of nodes (%v) does not match initial scale-up target (%v).", status.target, target)
		}
		framework.ExpectEqual(status.timestamp.Add(freshStatusLimit).Before(time.Now()), false)
		framework.ExpectEqual(status.status, caNoScaleUpStatus)
		framework.ExpectEqual(status.ready, status.target)
		nodes, err := e2enode.GetReadySchedulableNodes(f.ClientSet)
		framework.ExpectNoError(err)
		framework.ExpectEqual(len(nodes.Items), status.target+unmanagedNodes)
	})

	ginkgo.It("should increase cluster size if pods are pending due to host port conflict [Feature:ClusterSizeAutoscalingScaleUp]", func() {
		scheduling.CreateHostPortPods(f, "host-port", nodeCount+2, false)
		defer e2erc.DeleteRCAndWaitForGC(f.ClientSet, f.Namespace.Name, "host-port")

		framework.ExpectNoError(WaitForClusterSizeFunc(f.ClientSet,
			func(size int) bool { return size >= nodeCount+2 }, scaleUpTimeout))
		framework.ExpectNoError(waitForAllCaPodsReadyInNamespace(f, c))
	})

	ginkgo.It("should increase cluster size if pods are pending due to pod anti-affinity [Feature:ClusterSizeAutoscalingScaleUp]", func() {
		pods := nodeCount
		newPods := 2
		labels := map[string]string{
			"anti-affinity": "yes",
		}
		ginkgo.By("starting a pod with anti-affinity on each node")
		framework.ExpectNoError(runAntiAffinityPods(f, f.Namespace.Name, pods, "some-pod", labels, labels))
		defer e2erc.DeleteRCAndWaitForGC(f.ClientSet, f.Namespace.Name, "some-pod")
		framework.ExpectNoError(waitForAllCaPodsReadyInNamespace(f, c))

		ginkgo.By("scheduling extra pods with anti-affinity to existing ones")
		framework.ExpectNoError(runAntiAffinityPods(f, f.Namespace.Name, newPods, "extra-pod", labels, labels))
		defer e2erc.DeleteRCAndWaitForGC(f.ClientSet, f.Namespace.Name, "extra-pod")

		framework.ExpectNoError(waitForAllCaPodsReadyInNamespace(f, c))
		framework.ExpectNoError(e2enode.WaitForReadyNodes(c, nodeCount+newPods, scaleUpTimeout))
	})

	ginkgo.It("should increase cluster size if pod requesting EmptyDir volume is pending [Feature:ClusterSizeAutoscalingScaleUp]", func() {
		ginkgo.By("creating pods")
		pods := nodeCount
		newPods := 1
		labels := map[string]string{
			"anti-affinity": "yes",
		}
		framework.ExpectNoError(runAntiAffinityPods(f, f.Namespace.Name, pods, "some-pod", labels, labels))
		defer e2erc.DeleteRCAndWaitForGC(f.ClientSet, f.Namespace.Name, "some-pod")

		ginkgo.By("waiting for all pods before triggering scale up")
		framework.ExpectNoError(waitForAllCaPodsReadyInNamespace(f, c))

		ginkgo.By("creating a pod requesting EmptyDir")
		framework.ExpectNoError(runVolumeAntiAffinityPods(f, f.Namespace.Name, newPods, "extra-pod", labels, labels, emptyDirVolumes))
		defer e2erc.DeleteRCAndWaitForGC(f.ClientSet, f.Namespace.Name, "extra-pod")

		framework.ExpectNoError(waitForAllCaPodsReadyInNamespace(f, c))
		framework.ExpectNoError(e2enode.WaitForReadyNodes(c, nodeCount+newPods, scaleUpTimeout))
	})

	simpleScaleDownTest := func(unready int) {
		cleanup, err := addKubeSystemPdbs(f)
		defer cleanup()
		framework.ExpectNoError(err)

		ginkgo.By("Manually increase cluster size")
		increasedSize := 0
		newSizes := make(map[string]int)
		for key, val := range originalSizes {
			newSizes[key] = val + 2 + unready
			increasedSize += val + 2 + unready
		}
		setMigSizes(newSizes)
		framework.ExpectNoError(WaitForClusterSizeFuncWithUnready(f.ClientSet,
			func(size int) bool { return size >= increasedSize }, manualResizeTimeout, unready))

		ginkgo.By("Some node should be removed")
		framework.ExpectNoError(WaitForClusterSizeFuncWithUnready(f.ClientSet,
			func(size int) bool { return size < increasedSize }, scaleDownTimeout, unready))
	}

	ginkgo.It("should correctly scale down after a node is not needed [Feature:ClusterSizeAutoscalingScaleDown]",
		func() { simpleScaleDownTest(0) })

	ginkgo.It("should be able to scale down when rescheduling a pod is required and pdb allows for it[Feature:ClusterSizeAutoscalingScaleDown]", func() {
		runDrainTest(f, originalSizes, f.Namespace.Name, 1, 1, func(increasedSize int) {
			ginkgo.By("Some node should be removed")
			framework.ExpectNoError(WaitForClusterSizeFunc(f.ClientSet,
				func(size int) bool { return size < increasedSize }, scaleDownTimeout))
		})
	})

	ginkgo.It("shouldn't be able to scale down when rescheduling a pod is required, but pdb doesn't allow drain[Feature:ClusterSizeAutoscalingScaleDown]", func() {
		runDrainTest(f, originalSizes, f.Namespace.Name, 1, 0, func(increasedSize int) {
			ginkgo.By("No nodes should be removed")
			time.Sleep(scaleDownTimeout)
			nodes, err := e2enode.GetReadySchedulableNodes(f.ClientSet)
			framework.ExpectNoError(err)
			framework.ExpectEqual(len(nodes.Items), increasedSize)
		})
	})

	ginkgo.It("should be able to scale down by draining multiple pods one by one as dictated by pdb[Feature:ClusterSizeAutoscalingScaleDown]", func() {
		runDrainTest(f, originalSizes, f.Namespace.Name, 2, 1, func(increasedSize int) {
			ginkgo.By("Some node should be removed")
			framework.ExpectNoError(WaitForClusterSizeFunc(f.ClientSet,
				func(size int) bool { return size < increasedSize }, scaleDownTimeout))
		})
	})

	ginkgo.It("should be able to scale down by draining system pods with pdb[Feature:ClusterSizeAutoscalingScaleDown]", func() {
		runDrainTest(f, originalSizes, "kube-system", 2, 1, func(increasedSize int) {
			ginkgo.By("Some node should be removed")
			framework.ExpectNoError(WaitForClusterSizeFunc(f.ClientSet,
				func(size int) bool { return size < increasedSize }, scaleDownTimeout))
		})
	})

	ginkgo.It("shouldn't scale up when expendable pod is created [Feature:ClusterSizeAutoscalingScaleUp]", func() {
		defer createPriorityClasses(f)()
		// Create nodesCountAfterResize+1 pods allocating 0.7 allocatable on present nodes. One more node will have to be created.
		cleanupFunc := ReserveMemoryWithPriority(f, "memory-reservation", nodeCount+1, int(float64(nodeCount+1)*float64(0.7)*float64(memAllocatableMb)), false, time.Second, expendablePriorityClassName)
		defer cleanupFunc()
		ginkgo.By(fmt.Sprintf("Waiting for scale up hoping it won't happen, sleep for %s", scaleUpTimeout.String()))
		time.Sleep(scaleUpTimeout)
		// Verify that cluster size is not changed
		framework.ExpectNoError(WaitForClusterSizeFunc(f.ClientSet,
			func(size int) bool { return size == nodeCount }, time.Second))
	})

	ginkgo.It("should scale up when non expendable pod is created [Feature:ClusterSizeAutoscalingScaleUp]", func() {
		defer createPriorityClasses(f)()
		// Create nodesCountAfterResize+1 pods allocating 0.7 allocatable on present nodes. One more node will have to be created.
		cleanupFunc := ReserveMemoryWithPriority(f, "memory-reservation", nodeCount+1, int(float64(nodeCount+1)*float64(0.7)*float64(memAllocatableMb)), true, scaleUpTimeout, highPriorityClassName)
		defer cleanupFunc()
		// Verify that cluster size is not changed
		framework.ExpectNoError(WaitForClusterSizeFunc(f.ClientSet,
			func(size int) bool { return size > nodeCount }, time.Second))
	})

	ginkgo.It("shouldn't scale up when expendable pod is preempted [Feature:ClusterSizeAutoscalingScaleUp]", func() {
		defer createPriorityClasses(f)()
		// Create nodesCountAfterResize pods allocating 0.7 allocatable on present nodes - one pod per node.
		cleanupFunc1 := ReserveMemoryWithPriority(f, "memory-reservation1", nodeCount, int(float64(nodeCount)*float64(0.7)*float64(memAllocatableMb)), true, defaultTimeout, expendablePriorityClassName)
		defer cleanupFunc1()
		// Create nodesCountAfterResize pods allocating 0.7 allocatable on present nodes - one pod per node. Pods created here should preempt pods created above.
		cleanupFunc2 := ReserveMemoryWithPriority(f, "memory-reservation2", nodeCount, int(float64(nodeCount)*float64(0.7)*float64(memAllocatableMb)), true, defaultTimeout, highPriorityClassName)
		defer cleanupFunc2()
		framework.ExpectNoError(WaitForClusterSizeFunc(f.ClientSet,
			func(size int) bool { return size == nodeCount }, time.Second))
	})

	ginkgo.It("should scale down when expendable pod is running [Feature:ClusterSizeAutoscalingScaleDown]", func() {
		defer createPriorityClasses(f)()
		increasedSize := manuallyIncreaseClusterSize(f, originalSizes)
		// Create increasedSize pods allocating 0.7 allocatable on present nodes - one pod per node.
		cleanupFunc := ReserveMemoryWithPriority(f, "memory-reservation", increasedSize, int(float64(increasedSize)*float64(0.7)*float64(memAllocatableMb)), true, scaleUpTimeout, expendablePriorityClassName)
		defer cleanupFunc()
		ginkgo.By("Waiting for scale down")
		framework.ExpectNoError(WaitForClusterSizeFunc(f.ClientSet,
			func(size int) bool { return size == nodeCount }, scaleDownTimeout))
	})

	ginkgo.It("shouldn't scale down when non expendable pod is running [Feature:ClusterSizeAutoscalingScaleDown]", func() {
		defer createPriorityClasses(f)()
		increasedSize := manuallyIncreaseClusterSize(f, originalSizes)
		// Create increasedSize pods allocating 0.7 allocatable on present nodes - one pod per node.
		cleanupFunc := ReserveMemoryWithPriority(f, "memory-reservation", increasedSize, int(float64(increasedSize)*float64(0.7)*float64(memAllocatableMb)), true, scaleUpTimeout, highPriorityClassName)
		defer cleanupFunc()
		ginkgo.By(fmt.Sprintf("Waiting for scale down hoping it won't happen, sleep for %s", scaleDownTimeout.String()))
		time.Sleep(scaleDownTimeout)
		framework.ExpectNoError(WaitForClusterSizeFunc(f.ClientSet,
			func(size int) bool { return size == increasedSize }, time.Second))
	})
})

func runDrainTest(f *framework.Framework, migSizes map[string]int, namespace string, podsPerNode, pdbSize int, verifyFunction func(int)) {
	increasedSize := manuallyIncreaseClusterSize(f, migSizes)

	nodes, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{FieldSelector: fields.Set{
		"spec.unschedulable": "false",
	}.AsSelector().String()})
	framework.ExpectNoError(err)
	numPods := len(nodes.Items) * podsPerNode
	testID := string(uuid.NewUUID()) // So that we can label and find pods
	labelMap := map[string]string{"test_id": testID}
	framework.ExpectNoError(runReplicatedPodOnEachNode(f, nodes.Items, namespace, podsPerNode, "reschedulable-pods", labelMap, 0))

	defer e2erc.DeleteRCAndWaitForGC(f.ClientSet, namespace, "reschedulable-pods")

	ginkgo.By("Create a PodDisruptionBudget")
	minAvailable := intstr.FromInt(numPods - pdbSize)
	pdb := &policyv1beta1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test_pdb",
			Namespace: namespace,
		},
		Spec: policyv1beta1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: labelMap},
			MinAvailable: &minAvailable,
		},
	}
	_, err = f.ClientSet.PolicyV1beta1().PodDisruptionBudgets(namespace).Create(context.TODO(), pdb, metav1.CreateOptions{})

	defer func() {
		f.ClientSet.PolicyV1beta1().PodDisruptionBudgets(namespace).Delete(context.TODO(), pdb.Name, metav1.DeleteOptions{})
	}()

	framework.ExpectNoError(err)
	verifyFunction(increasedSize)
}

func reserveMemory(f *framework.Framework, id string, replicas, megabytes int, expectRunning bool, timeout time.Duration, selector map[string]string, tolerations []v1.Toleration, priorityClassName string) func() error {
	ginkgo.By(fmt.Sprintf("Running RC which reserves %v MB of memory", megabytes))
	request := int64(1024 * 1024 * megabytes / replicas)
	config := &testutils.RCConfig{
		Client:            f.ClientSet,
		Name:              id,
		Namespace:         f.Namespace.Name,
		Timeout:           timeout,
		Image:             imageutils.GetPauseImageName(),
		Replicas:          replicas,
		MemRequest:        request,
		NodeSelector:      selector,
		Tolerations:       tolerations,
		PriorityClassName: priorityClassName,
	}
	for start := time.Now(); time.Since(start) < rcCreationRetryTimeout; time.Sleep(rcCreationRetryDelay) {
		err := e2erc.RunRC(*config)
		if err != nil && strings.Contains(err.Error(), "Error creating replication controller") {
			klog.Warningf("Failed to create memory reservation: %v", err)
			continue
		}
		if expectRunning {
			framework.ExpectNoError(err)
		}
		return func() error {
			return e2erc.DeleteRCAndWaitForGC(f.ClientSet, f.Namespace.Name, id)
		}
	}
	framework.Failf("Failed to reserve memory within timeout")
	return nil
}

// ReserveMemoryWithPriority creates a replication controller with pods with priority that, in summation,
// request the specified amount of memory.
func ReserveMemoryWithPriority(f *framework.Framework, id string, replicas, megabytes int, expectRunning bool, timeout time.Duration, priorityClassName string) func() error {
	return reserveMemory(f, id, replicas, megabytes, expectRunning, timeout, nil, nil, priorityClassName)
}

// ReserveMemoryWithSelectorAndTolerations creates a replication controller with pods with node selector that, in summation,
// request the specified amount of memory.
func ReserveMemoryWithSelectorAndTolerations(f *framework.Framework, id string, replicas, megabytes int, expectRunning bool, timeout time.Duration, selector map[string]string, tolerations []v1.Toleration) func() error {
	return reserveMemory(f, id, replicas, megabytes, expectRunning, timeout, selector, tolerations, "")
}

// ReserveMemory creates a replication controller with pods that, in summation,
// request the specified amount of memory.
func ReserveMemory(f *framework.Framework, id string, replicas, megabytes int, expectRunning bool, timeout time.Duration) func() error {
	return reserveMemory(f, id, replicas, megabytes, expectRunning, timeout, nil, nil, "")
}

// WaitForClusterSizeFunc waits until the cluster size matches the given function.
func WaitForClusterSizeFunc(c clientset.Interface, sizeFunc func(int) bool, timeout time.Duration) error {
	return WaitForClusterSizeFuncWithUnready(c, sizeFunc, timeout, 0)
}

// WaitForClusterSizeFuncWithUnready waits until the cluster size matches the given function and assumes some unready nodes.
func WaitForClusterSizeFuncWithUnready(c clientset.Interface, sizeFunc func(int) bool, timeout time.Duration, expectedUnready int) error {
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(20 * time.Second) {
		nodes, err := c.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{FieldSelector: fields.Set{
			"spec.unschedulable": "false",
		}.AsSelector().String()})
		if err != nil {
			klog.Warningf("Failed to list nodes: %v", err)
			continue
		}
		numNodes := len(nodes.Items)

		// Filter out not-ready nodes.
		e2enode.Filter(nodes, func(node v1.Node) bool {
			return e2enode.IsConditionSetAsExpected(&node, v1.NodeReady, true)
		})
		numReady := len(nodes.Items)

		if numNodes == numReady+expectedUnready && sizeFunc(numNodes) {
			klog.Infof("Cluster has reached the desired size")
			return nil
		}
		klog.Infof("Waiting for cluster with func, current size %d, not ready nodes %d", numNodes, numNodes-numReady)
	}
	return fmt.Errorf("timeout waiting %v for appropriate cluster size", timeout)
}

func waitForCaPodsReadyInNamespace(f *framework.Framework, c clientset.Interface, tolerateUnreadyCount int) error {
	var notready []string
	for start := time.Now(); time.Now().Before(start.Add(scaleUpTimeout)); time.Sleep(20 * time.Second) {
		pods, err := c.CoreV1().Pods(f.Namespace.Name).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to get pods: %v", err)
		}
		notready = make([]string, 0)
		for _, pod := range pods.Items {
			ready := false
			for _, c := range pod.Status.Conditions {
				if c.Type == v1.PodReady && c.Status == v1.ConditionTrue {
					ready = true
				}
			}
			// Failed pods in this context generally mean that they have been
			// double scheduled onto a node, but then failed a constraint check.
			if pod.Status.Phase == v1.PodFailed {
				klog.Warningf("Pod has failed: %v", pod)
			}
			if !ready && pod.Status.Phase != v1.PodFailed {
				notready = append(notready, pod.Name)
			}
		}
		if len(notready) <= tolerateUnreadyCount {
			klog.Infof("sufficient number of pods ready. Tolerating %d unready", tolerateUnreadyCount)
			return nil
		}
		klog.Infof("Too many pods are not ready yet: %v", notready)
	}
	klog.Info("Timeout on waiting for pods being ready")
	klog.Info(framework.RunKubectlOrDie(f.Namespace.Name, "get", "pods", "-o", "json", "--all-namespaces"))
	klog.Info(framework.RunKubectlOrDie(f.Namespace.Name, "get", "nodes", "-o", "json"))

	// Some pods are still not running.
	return fmt.Errorf("Too many pods are still not running: %v", notready)
}

func waitForAllCaPodsReadyInNamespace(f *framework.Framework, c clientset.Interface) error {
	return waitForCaPodsReadyInNamespace(f, c, 0)
}

func setMigSizes(sizes map[string]int) bool {
	madeChanges := false
	for mig, desiredSize := range sizes {
		currentSize, err := framework.GroupSize(mig)
		framework.ExpectNoError(err)
		if desiredSize != currentSize {
			ginkgo.By(fmt.Sprintf("Setting size of %s to %d", mig, desiredSize))
			err = framework.ResizeGroup(mig, int32(desiredSize))
			framework.ExpectNoError(err)
			madeChanges = true
		}
	}
	return madeChanges
}

func makeNodeUnschedulable(c clientset.Interface, node *v1.Node) error {
	ginkgo.By(fmt.Sprintf("Taint node %s", node.Name))
	for j := 0; j < 3; j++ {
		freshNode, err := c.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for _, taint := range freshNode.Spec.Taints {
			if taint.Key == disabledTaint {
				return nil
			}
		}
		freshNode.Spec.Taints = append(freshNode.Spec.Taints, v1.Taint{
			Key:    disabledTaint,
			Value:  "DisabledForTest",
			Effect: v1.TaintEffectNoSchedule,
		})
		_, err = c.CoreV1().Nodes().Update(context.TODO(), freshNode, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
		klog.Warningf("Got 409 conflict when trying to taint node, retries left: %v", 3-j)
	}
	return fmt.Errorf("Failed to taint node in allowed number of retries")
}

// CriticalAddonsOnlyError implements the `error` interface, and signifies the
// presence of the `CriticalAddonsOnly` taint on the node.
type CriticalAddonsOnlyError struct{}

func (CriticalAddonsOnlyError) Error() string {
	return fmt.Sprintf("CriticalAddonsOnly taint found on node")
}

func makeNodeSchedulable(c clientset.Interface, node *v1.Node, failOnCriticalAddonsOnly bool) error {
	ginkgo.By(fmt.Sprintf("Remove taint from node %s", node.Name))
	for j := 0; j < 3; j++ {
		freshNode, err := c.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		var newTaints []v1.Taint
		for _, taint := range freshNode.Spec.Taints {
			if failOnCriticalAddonsOnly && taint.Key == criticalAddonsOnlyTaint {
				return CriticalAddonsOnlyError{}
			}
			if taint.Key != disabledTaint {
				newTaints = append(newTaints, taint)
			}
		}

		if len(newTaints) == len(freshNode.Spec.Taints) {
			return nil
		}
		freshNode.Spec.Taints = newTaints
		_, err = c.CoreV1().Nodes().Update(context.TODO(), freshNode, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
		klog.Warningf("Got 409 conflict when trying to taint node, retries left: %v", 3-j)
	}
	return fmt.Errorf("Failed to remove taint from node in allowed number of retries")
}

// Create an RC running a given number of pods with anti-affinity
func runAntiAffinityPods(f *framework.Framework, namespace string, pods int, id string, podLabels, antiAffinityLabels map[string]string) error {
	config := &testutils.RCConfig{
		Affinity:  buildAntiAffinity(antiAffinityLabels),
		Client:    f.ClientSet,
		Name:      id,
		Namespace: namespace,
		Timeout:   scaleUpTimeout,
		Image:     imageutils.GetPauseImageName(),
		Replicas:  pods,
		Labels:    podLabels,
	}
	err := e2erc.RunRC(*config)
	if err != nil {
		return err
	}
	_, err = f.ClientSet.CoreV1().ReplicationControllers(namespace).Get(context.TODO(), id, metav1.GetOptions{})
	if err != nil {
		return err
	}
	return nil
}

func runVolumeAntiAffinityPods(f *framework.Framework, namespace string, pods int, id string, podLabels, antiAffinityLabels map[string]string, volumes []v1.Volume) error {
	config := &testutils.RCConfig{
		Affinity:  buildAntiAffinity(antiAffinityLabels),
		Volumes:   volumes,
		Client:    f.ClientSet,
		Name:      id,
		Namespace: namespace,
		Timeout:   scaleUpTimeout,
		Image:     imageutils.GetPauseImageName(),
		Replicas:  pods,
		Labels:    podLabels,
	}
	err := e2erc.RunRC(*config)
	if err != nil {
		return err
	}
	_, err = f.ClientSet.CoreV1().ReplicationControllers(namespace).Get(context.TODO(), id, metav1.GetOptions{})
	if err != nil {
		return err
	}
	return nil
}

var emptyDirVolumes = []v1.Volume{
	{
		Name: "empty-volume",
		VolumeSource: v1.VolumeSource{
			EmptyDir: &v1.EmptyDirVolumeSource{},
		},
	},
}

func buildAntiAffinity(labels map[string]string) *v1.Affinity {
	return &v1.Affinity{
		PodAntiAffinity: &v1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: labels,
					},
					TopologyKey: "kubernetes.io/hostname",
				},
			},
		},
	}
}

// Create an RC running a given number of pods on each node without adding any constraint forcing
// such pod distribution. This is meant to create a bunch of underutilized (but not unused) nodes
// with pods that can be rescheduled on different nodes.
// This is achieved using the following method:
// 1. disable scheduling on each node
// 2. create an empty RC
// 3. for each node:
// 3a. enable scheduling on that node
// 3b. increase number of replicas in RC by podsPerNode
func runReplicatedPodOnEachNode(f *framework.Framework, nodes []v1.Node, namespace string, podsPerNode int, id string, labels map[string]string, memRequest int64) error {
	ginkgo.By("Run a pod on each node")
	for _, node := range nodes {
		err := makeNodeUnschedulable(f.ClientSet, &node)

		defer func(n v1.Node) {
			makeNodeSchedulable(f.ClientSet, &n, false)
		}(node)

		if err != nil {
			return err
		}
	}
	config := &testutils.RCConfig{
		Client:     f.ClientSet,
		Name:       id,
		Namespace:  namespace,
		Timeout:    defaultTimeout,
		Image:      imageutils.GetPauseImageName(),
		Replicas:   0,
		Labels:     labels,
		MemRequest: memRequest,
	}
	err := e2erc.RunRC(*config)
	if err != nil {
		return err
	}
	rc, err := f.ClientSet.CoreV1().ReplicationControllers(namespace).Get(context.TODO(), id, metav1.GetOptions{})
	if err != nil {
		return err
	}
	for i, node := range nodes {
		err = makeNodeSchedulable(f.ClientSet, &node, false)
		if err != nil {
			return err
		}

		// Update replicas count, to create new pods that will be allocated on node
		// (we retry 409 errors in case rc reference got out of sync)
		for j := 0; j < 3; j++ {
			*rc.Spec.Replicas = int32((i + 1) * podsPerNode)
			rc, err = f.ClientSet.CoreV1().ReplicationControllers(namespace).Update(context.TODO(), rc, metav1.UpdateOptions{})
			if err == nil {
				break
			}
			if !apierrors.IsConflict(err) {
				return err
			}
			klog.Warningf("Got 409 conflict when trying to scale RC, retries left: %v", 3-j)
			rc, err = f.ClientSet.CoreV1().ReplicationControllers(namespace).Get(context.TODO(), id, metav1.GetOptions{})
			if err != nil {
				return err
			}
		}

		err = wait.PollImmediate(5*time.Second, podTimeout, func() (bool, error) {
			rc, err = f.ClientSet.CoreV1().ReplicationControllers(namespace).Get(context.TODO(), id, metav1.GetOptions{})
			if err != nil || rc.Status.ReadyReplicas < int32((i+1)*podsPerNode) {
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			return fmt.Errorf("failed to coerce RC into spawning a pod on node %s within timeout", node.Name)
		}
		err = makeNodeUnschedulable(f.ClientSet, &node)
		if err != nil {
			return err
		}
	}
	return nil
}

// Increase cluster size by newNodesForScaledownTests to create some unused nodes
// that can be later removed by cluster autoscaler.
func manuallyIncreaseClusterSize(f *framework.Framework, originalSizes map[string]int) int {
	ginkgo.By("Manually increase cluster size")
	increasedSize := 0
	newSizes := make(map[string]int)
	for key, val := range originalSizes {
		newSizes[key] = val + newNodesForScaledownTests
		increasedSize += val + newNodesForScaledownTests
	}
	setMigSizes(newSizes)

	checkClusterSize := func(size int) bool {
		if size >= increasedSize {
			return true
		}
		resized := setMigSizes(newSizes)
		if resized {
			klog.Warning("Unexpected node group size while waiting for cluster resize. Setting size to target again.")
		}
		return false
	}

	framework.ExpectNoError(WaitForClusterSizeFunc(f.ClientSet, checkClusterSize, manualResizeTimeout))
	return increasedSize
}

type scaleUpStatus struct {
	status    string
	ready     int
	target    int
	timestamp time.Time
}

// Try to get timestamp from status.
// Status configmap is not parsing-friendly, so evil regexpery follows.
func getStatusTimestamp(status string) (time.Time, error) {
	timestampMatcher, err := regexp.Compile("Cluster-autoscaler status at \\s*([0-9\\-]+ [0-9]+:[0-9]+:[0-9]+\\.[0-9]+ \\+[0-9]+ [A-Za-z]+)")
	if err != nil {
		return time.Time{}, err
	}

	timestampMatch := timestampMatcher.FindStringSubmatch(status)
	if len(timestampMatch) < 2 {
		return time.Time{}, fmt.Errorf("Failed to parse CA status timestamp, raw status: %v", status)
	}

	timestamp, err := time.Parse(timestampFormat, timestampMatch[1])
	if err != nil {
		return time.Time{}, err
	}
	return timestamp, nil
}

// Try to get scaleup statuses of all node groups.
// Status configmap is not parsing-friendly, so evil regexpery follows.
func getScaleUpStatus(c clientset.Interface) (*scaleUpStatus, error) {
	configMap, err := c.CoreV1().ConfigMaps("kube-system").Get(context.TODO(), "cluster-autoscaler-status", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	status, ok := configMap.Data["status"]
	if !ok {
		return nil, fmt.Errorf("Status information not found in configmap")
	}

	timestamp, err := getStatusTimestamp(status)
	if err != nil {
		return nil, err
	}

	matcher, err := regexp.Compile("s*ScaleUp:\\s*([A-Za-z]+)\\s*\\(ready=([0-9]+)\\s*cloudProviderTarget=([0-9]+)\\s*\\)")
	if err != nil {
		return nil, err
	}
	matches := matcher.FindAllStringSubmatch(status, -1)
	if len(matches) < 1 {
		return nil, fmt.Errorf("Failed to parse CA status configmap, raw status: %v", status)
	}

	result := scaleUpStatus{
		status:    caNoScaleUpStatus,
		ready:     0,
		target:    0,
		timestamp: timestamp,
	}
	for _, match := range matches {
		if match[1] == caOngoingScaleUpStatus {
			result.status = caOngoingScaleUpStatus
		}
		newReady, err := strconv.Atoi(match[2])
		if err != nil {
			return nil, err
		}
		result.ready += newReady
		newTarget, err := strconv.Atoi(match[3])
		if err != nil {
			return nil, err
		}
		result.target += newTarget
	}
	klog.Infof("Cluster-Autoscaler scale-up status: %v (%v, %v)", result.status, result.ready, result.target)
	return &result, nil
}

func waitForScaleUpStatus(c clientset.Interface, cond func(s *scaleUpStatus) bool, timeout time.Duration) (*scaleUpStatus, error) {
	var finalErr error
	var status *scaleUpStatus
	err := wait.PollImmediate(5*time.Second, timeout, func() (bool, error) {
		status, finalErr = getScaleUpStatus(c)
		if finalErr != nil {
			return false, nil
		}
		if status.timestamp.Add(freshStatusLimit).Before(time.Now()) {
			// stale status
			finalErr = fmt.Errorf("Status too old")
			return false, nil
		}
		return cond(status), nil
	})
	if err != nil {
		err = fmt.Errorf("Failed to find expected scale up status: %v, last status: %v, final err: %v", err, status, finalErr)
	}
	return status, err
}

// This is a temporary fix to allow CA to migrate some kube-system pods
// TODO: Remove this when the PDB is added for some of those components
func addKubeSystemPdbs(f *framework.Framework) (func(), error) {
	ginkgo.By("Create PodDisruptionBudgets for kube-system components, so they can be migrated if required")

	var newPdbs []string
	cleanup := func() {
		var finalErr error
		for _, newPdbName := range newPdbs {
			ginkgo.By(fmt.Sprintf("Delete PodDisruptionBudget %v", newPdbName))
			err := f.ClientSet.PolicyV1beta1().PodDisruptionBudgets("kube-system").Delete(context.TODO(), newPdbName, metav1.DeleteOptions{})
			if err != nil {
				// log error, but attempt to remove other pdbs
				klog.Errorf("Failed to delete PodDisruptionBudget %v, err: %v", newPdbName, err)
				finalErr = err
			}
		}
		if finalErr != nil {
			framework.Failf("Error during PodDisruptionBudget cleanup: %v", finalErr)
		}
	}

	type pdbInfo struct {
		label        string
		minAvailable int
	}
	pdbsToAdd := []pdbInfo{
		{label: "kube-dns", minAvailable: 1},
		{label: "kube-dns-autoscaler", minAvailable: 0},
		{label: "metrics-server", minAvailable: 0},
		{label: "kubernetes-dashboard", minAvailable: 0},
		{label: "glbc", minAvailable: 0},
	}
	for _, pdbData := range pdbsToAdd {
		ginkgo.By(fmt.Sprintf("Create PodDisruptionBudget for %v", pdbData.label))
		labelMap := map[string]string{"k8s-app": pdbData.label}
		pdbName := fmt.Sprintf("test-pdb-for-%v", pdbData.label)
		minAvailable := intstr.FromInt(pdbData.minAvailable)
		pdb := &policyv1beta1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pdbName,
				Namespace: "kube-system",
			},
			Spec: policyv1beta1.PodDisruptionBudgetSpec{
				Selector:     &metav1.LabelSelector{MatchLabels: labelMap},
				MinAvailable: &minAvailable,
			},
		}
		_, err := f.ClientSet.PolicyV1beta1().PodDisruptionBudgets("kube-system").Create(context.TODO(), pdb, metav1.CreateOptions{})
		newPdbs = append(newPdbs, pdbName)

		if err != nil {
			return cleanup, err
		}
	}
	return cleanup, nil
}

func createPriorityClasses(f *framework.Framework) func() {
	priorityClasses := map[string]int32{
		expendablePriorityClassName: -15,
		highPriorityClassName:       1000,
	}
	for className, priority := range priorityClasses {
		_, err := f.ClientSet.SchedulingV1().PriorityClasses().Create(context.TODO(), &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: className}, Value: priority}, metav1.CreateOptions{})
		if err != nil {
			klog.Errorf("Error creating priority class: %v", err)
		}
		framework.ExpectEqual(err == nil || apierrors.IsAlreadyExists(err), true)
	}

	return func() {
		for className := range priorityClasses {
			err := f.ClientSet.SchedulingV1().PriorityClasses().Delete(context.TODO(), className, metav1.DeleteOptions{})
			if err != nil {
				klog.Errorf("Error deleting priority class: %v", err)
			}
		}
	}
}
