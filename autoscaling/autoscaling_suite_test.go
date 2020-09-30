package autoscaling_test

import (
	"flag"
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"k8s.io/klog"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/config"

	_ "github.com/benmoss/autoscaler-tests/providers/clusterapi"
)

func init() {
	config.CopyFlags(config.Flags, flag.CommandLine)
	framework.RegisterCommonFlags(flag.CommandLine)
	framework.RegisterClusterFlags(flag.CommandLine)
	klog.InitFlags(nil)
}

func TestAutoscaling(t *testing.T) {
	framework.AfterReadingAllFlags(&framework.TestContext)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Autoscaling Suite")
}
