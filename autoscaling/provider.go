package autoscaling

import (
	"k8s.io/kubernetes/test/e2e/framework"
)

type Provider interface {
	framework.ProviderInterface
	EnableAutoscaler(nodeGroup string, minSize, maxSize int) error
	DisableAutoscaler(nodeGroup string) error
}
