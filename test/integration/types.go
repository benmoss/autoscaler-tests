package integration

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type Framework struct {
	ClientConfig clientcmd.ClientConfig
	ClientSet    clientset.Interface
	Namespace    *v1.Namespace
	Provider     Provider
	T            *testing.T
}

type Provider interface {
	FrameworkBeforeEach(f *Framework)
	FrameworkAfterEach(f *Framework)

	DisableAutoscaler(nodeGroup string) error
	EnableAutoscaler(nodeGroup string, minSize, maxSize int) error
	GroupSize(group string) (int, error)
	ResizeGroup(group string, size int32) error
}
