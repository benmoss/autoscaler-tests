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
	"encoding/json"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2erc "k8s.io/kubernetes/test/e2e/framework/rc"
	testutils "k8s.io/kubernetes/test/utils"
	imageutils "k8s.io/kubernetes/test/utils/image"

	"github.com/onsi/ginkgo"
)

const (
	memoryReservationTimeout = 5 * time.Minute
	largeResizeTimeout       = 8 * time.Minute
	largeScaleUpTimeout      = 10 * time.Minute
	maxNodes                 = 1000
)

type clusterPredicates struct {
	nodes int
}

type scaleUpTestConfig struct {
	initialNodes   int
	initialPods    int
	extraPods      *testutils.RCConfig
	expectedResult *clusterPredicates
}

var _ = ginkgo.Describe("Cluster size autoscaler scalability [Slow]", func() {
	f := framework.NewDefaultFramework("autoscaling")
	var c clientset.Interface
	var nodeCount int
	var memCapacityMb int
	// var coresPerNode int
	var provider Provider
	var originalSizes map[string]int
	var sum int

	ginkgo.BeforeEach(func() {
		c = f.ClientSet
		var ok bool
		provider, ok = framework.TestContext.CloudConfig.Provider.(Provider)
		if !ok {
			ginkgo.Skip("provider does not support autoscaler tests")
		}

		if originalSizes == nil {
			originalSizes = make(map[string]int)
			sum = 0
			for _, mig := range strings.Split(framework.TestContext.CloudConfig.NodeInstanceGroup, ",") {
				size, err := provider.GroupSize(mig)
				framework.ExpectNoError(err)
				ginkgo.By(fmt.Sprintf("Initial size of %s: %d", mig, size))
				originalSizes[mig] = size
				sum += size
			}
		}

		framework.ExpectNoError(e2enode.WaitForReadyNodes(c, sum, scaleUpTimeout))

		nodes, err := e2enode.GetReadySchedulableNodes(f.ClientSet)
		framework.ExpectNoError(err)

		nodeCount = len(nodes.Items)
		// cpu := nodes.Items[0].Status.Capacity[v1.ResourceCPU]
		mem := nodes.Items[0].Status.Capacity[v1.ResourceMemory]
		// coresPerNode = int((&cpu).MilliValue() / 1000)
		memCapacityMb = int((&mem).Value() / 1024 / 1024)
		framework.ExpectNoError(provider.EnableAutoscaler("default-pool", 3, 5))
	})

	ginkgo.AfterEach(func() {
		framework.ExpectNoError(provider.DisableAutoscaler("default-pool"))

		ginkgo.By("Restoring initial size of the cluster")
		framework.ExpectNoError(provider.ResetInstanceGroups())
	})

	ginkgo.It("should scale up at all [Feature:ClusterAutoscalerScalability1]", func() {
		perNodeReservation := int(float64(memCapacityMb) * 0.95)
		replicasPerNode := 10

		additionalNodes := maxNodes - nodeCount
		replicas := additionalNodes * replicasPerNode
		additionalReservation := additionalNodes * perNodeReservation

		// saturate cluster
		reservationCleanup := ReserveMemory(f, "some-pod", nodeCount*2, nodeCount*perNodeReservation, true, memoryReservationTimeout)
		defer reservationCleanup()
		framework.ExpectNoError(waitForAllCaPodsReadyInNamespace(f, c))

		// configure pending pods & expected scale up
		rcConfig := reserveMemoryRCConfig(f, "extra-pod-1", replicas, additionalReservation, largeScaleUpTimeout)
		expectedResult := createClusterPredicates(nodeCount + additionalNodes)
		config := createScaleUpTestConfig(nodeCount, nodeCount, rcConfig, expectedResult)

		// run test
		testCleanup := simpleScaleUpTest(f, config)
		defer testCleanup()
	})
})

func anyKey(input map[string]int) string {
	for k := range input {
		return k
	}
	return ""
}

func simpleScaleUpTestWithTolerance(f *framework.Framework, config *scaleUpTestConfig, tolerateMissingNodeCount int, tolerateMissingPodCount int) func() error {
	// resize cluster to start size
	// run rc based on config
	ginkgo.By(fmt.Sprintf("Running RC %v from config", config.extraPods.Name))
	start := time.Now()
	framework.ExpectNoError(e2erc.RunRC(*config.extraPods))
	// check results
	if tolerateMissingNodeCount > 0 {
		// Tolerate some number of nodes not to be created.
		minExpectedNodeCount := config.expectedResult.nodes - tolerateMissingNodeCount
		framework.ExpectNoError(WaitForClusterSizeFunc(f.ClientSet,
			func(size int) bool { return size >= minExpectedNodeCount }, scaleUpTimeout))
	} else {
		framework.ExpectNoError(e2enode.WaitForReadyNodes(f.ClientSet, config.expectedResult.nodes, scaleUpTimeout))
	}
	klog.Infof("cluster is increased")
	if tolerateMissingPodCount > 0 {
		framework.ExpectNoError(waitForCaPodsReadyInNamespace(f, f.ClientSet, tolerateMissingPodCount))
	} else {
		framework.ExpectNoError(waitForAllCaPodsReadyInNamespace(f, f.ClientSet))
	}
	timeTrack(start, fmt.Sprintf("Scale up to %v", config.expectedResult.nodes))
	return func() error {
		return e2erc.DeleteRCAndWaitForGC(f.ClientSet, f.Namespace.Name, config.extraPods.Name)
	}
}

func simpleScaleUpTest(f *framework.Framework, config *scaleUpTestConfig) func() error {
	return simpleScaleUpTestWithTolerance(f, config, 0, 0)
}

func reserveMemoryRCConfig(f *framework.Framework, id string, replicas, megabytes int, timeout time.Duration) *testutils.RCConfig {
	return &testutils.RCConfig{
		Client:     f.ClientSet,
		Name:       id,
		Namespace:  f.Namespace.Name,
		Timeout:    timeout,
		Image:      imageutils.GetPauseImageName(),
		Replicas:   replicas,
		MemRequest: int64(1024 * 1024 * megabytes / replicas),
	}
}

func createScaleUpTestConfig(nodes, pods int, extraPods *testutils.RCConfig, expectedResult *clusterPredicates) *scaleUpTestConfig {
	return &scaleUpTestConfig{
		initialNodes:   nodes,
		initialPods:    pods,
		extraPods:      extraPods,
		expectedResult: expectedResult,
	}
}

func createClusterPredicates(nodes int) *clusterPredicates {
	return &clusterPredicates{
		nodes: nodes,
	}
}

func addAnnotation(f *framework.Framework, nodes []v1.Node, key, value string) error {
	for _, node := range nodes {
		oldData, err := json.Marshal(node)
		if err != nil {
			return err
		}

		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}
		node.Annotations[key] = value

		newData, err := json.Marshal(node)
		if err != nil {
			return err
		}

		patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, v1.Node{})
		if err != nil {
			return err
		}

		_, err = f.ClientSet.CoreV1().Nodes().Patch(context.TODO(), string(node.Name), types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
		if err != nil {
			return err
		}
	}
	return nil
}

func createHostPortPodsWithMemory(f *framework.Framework, id string, replicas, port, megabytes int, timeout time.Duration) func() error {
	ginkgo.By(fmt.Sprintf("Running RC which reserves host port and memory"))
	request := int64(1024 * 1024 * megabytes / replicas)
	config := &testutils.RCConfig{
		Client:     f.ClientSet,
		Name:       id,
		Namespace:  f.Namespace.Name,
		Timeout:    timeout,
		Image:      imageutils.GetPauseImageName(),
		Replicas:   replicas,
		HostPorts:  map[string]int{"port1": port},
		MemRequest: request,
	}
	err := e2erc.RunRC(*config)
	framework.ExpectNoError(err)
	return func() error {
		return e2erc.DeleteRCAndWaitForGC(f.ClientSet, f.Namespace.Name, id)
	}
}

type podBatch struct {
	numNodes    int
	podsPerNode int
}

// distributeLoad distributes the pods in the way described by podDostribution,
// assuming all pods will have the same memory reservation and all nodes the same
// memory capacity. This allows us generate the load on the cluster in the exact
// way that we want.
//
// To achieve this we do the following:
// 1. Create replication controllers that eat up all the space that should be
// empty after setup, making sure they end up on different nodes by specifying
// conflicting host port
// 2. Create targer RC that will generate the load on the cluster
// 3. Remove the rcs created in 1.
func distributeLoad(f *framework.Framework, namespace string, id string, podDistribution []podBatch,
	podMemRequestMegabytes int, nodeMemCapacity int, labels map[string]string, timeout time.Duration) func() error {
	port := 8013
	// Create load-distribution RCs with one pod per node, reserving all remaining
	// memory to force the distribution of pods for the target RCs.
	// The load-distribution RCs will be deleted on function return.
	totalPods := 0
	for i, podBatch := range podDistribution {
		totalPods += podBatch.numNodes * podBatch.podsPerNode
		remainingMem := nodeMemCapacity - podBatch.podsPerNode*podMemRequestMegabytes
		replicas := podBatch.numNodes
		cleanup := createHostPortPodsWithMemory(f, fmt.Sprintf("load-distribution%d", i), replicas, port, remainingMem*replicas, timeout)
		defer cleanup()
	}
	framework.ExpectNoError(waitForAllCaPodsReadyInNamespace(f, f.ClientSet))
	// Create the target RC
	rcConfig := reserveMemoryRCConfig(f, id, totalPods, totalPods*podMemRequestMegabytes, timeout)
	framework.ExpectNoError(e2erc.RunRC(*rcConfig))
	framework.ExpectNoError(waitForAllCaPodsReadyInNamespace(f, f.ClientSet))
	return func() error {
		return e2erc.DeleteRCAndWaitForGC(f.ClientSet, f.Namespace.Name, id)
	}
}

func timeTrack(start time.Time, name string) {
	elapsed := time.Since(start)
	klog.Infof("%s took %s", name, elapsed)
}
