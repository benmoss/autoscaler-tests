package autoscaling

import (
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
)

type Provider interface {
	framework.ProviderInterface
	EnableAutoscaler(nodeGroup string, minSize, maxSize int) error
	DisableAutoscaler(nodeGroup string) error
	WaitForReadyNodes(client kubernetes.Interface, timeout time.Duration) error
	ResetInstanceGroups(map[string]int) error
}
