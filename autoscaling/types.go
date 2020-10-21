package autoscaling

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
)

type Framework struct {
	Namespace      *v1.Namespace
	ClientSet      clientset.Interface
	ClientConfig   *restclient.Config
	Provider       Provider
	T              *testing.T
	KubeconfigPath string
}

type Provider interface {
	FrameworkBeforeEach(f *Framework)
	FrameworkAfterEach(f *Framework)

	DisableAutoscaler(nodeGroup string) error
	EnableAutoscaler(nodeGroup string, minSize, maxSize int) error
	GroupSize(group string) (int, error)
	ResizeGroup(group string, size int32) error
}
