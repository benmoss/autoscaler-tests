package clusterapi

import (
	"context"
	"flag"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/scale"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/auth"
	"k8s.io/utils/pointer"
)

const autoscalerName = "cluster-autoscaler"

var (
	capiManagementKubeConfig = flag.String(fmt.Sprintf("%s-%s", "capi-management", clientcmd.RecommendedConfigPathFlag), "", "Path to kubeconfig containing embedded authinfo for CAPI management cluster.")
	capiManagementNamespace  = flag.String("capi-management-namespace", "default", "Namespace in which the scalable resources are located")
	clusterAutoscalerImage   = flag.String("cluster-autoscaler-image", "", "Image to be used for the cluster autoscaler")

	namespace = &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: autoscalerName + "-",
		},
	}
	deployment = &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   autoscalerName,
			Labels: map[string]string{"app": autoscalerName},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: pointer.Int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": autoscalerName,
				},
			},
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": autoscalerName,
					},
				},
				Spec: apiv1.PodSpec{
					Containers: []apiv1.Container{
						{
							Name:    autoscalerName,
							Image:   "us.gcr.io/k8s-artifacts-prod/autoscaling/cluster-autoscaler:v1.18.1",
							Command: []string{"/cluster-autoscaler"},
							Args:    []string{"--cloud-provider=clusterapi"},
						},
					},
					Tolerations: []apiv1.Toleration{
						{
							Key:    "node-role.kubernetes.io/master",
							Effect: apiv1.TaintEffectNoSchedule,
						},
					},
					ServiceAccountName: autoscalerName,
				},
			},
		},
	}
	serviceAccount = &apiv1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: autoscalerName,
		},
	}
	clusterRole = &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: autoscalerName + "-",
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Verbs:     []string{"get", "list", "update", "watch", "create"},
				Resources: []string{"persistentvolumeclaims", "persistentvolumes", "pods", "replicationcontrollers", "nodes", "pods/eviction"},
			},
		},
	}
)

func init() {
	framework.RegisterProvider("clusterapi", factory)
}

func factory() (framework.ProviderInterface, error) {
	framework.Logf("Fetching cloud provider for %q\r", framework.TestContext.Provider)

	return &Provider{}, nil
}

type Provider struct {
	framework.NullProvider
	workloadClient          kubernetes.Interface
	managementClient        kubernetes.Interface
	machineDeploymentClient dynamic.ResourceInterface
	managementScaleClient   scale.ScalesGetter
	gvr                     schema.GroupVersionResource
	namespace               *v1.Namespace
}

func (p *Provider) FrameworkBeforeEach(f *framework.Framework) {
	var (
		managementConfig *rest.Config
		err              error
	)

	if *capiManagementKubeConfig != "" {
		managementConfig, err = clientcmd.BuildConfigFromFlags("", *capiManagementKubeConfig)
		framework.ExpectNoError(err)
		managementClient, err := kubernetes.NewForConfig(managementConfig)
		framework.ExpectNoError(err)

		p.managementClient = managementClient
	} else {
		framework.Logf("No management kubeconfig provided, assuming a self-managed cluster")
		p.managementClient = f.ClientSet
		p.workloadClient = f.ClientSet
		managementConfig = f.ClientConfig()
	}

	CAPIGroup := getCAPIGroup()
	CAPIVersion, err := getAPIGroupPreferredVersion(p.managementClient.(discovery.DiscoveryInterface), CAPIGroup)
	framework.ExpectNoError(err)

	framework.Logf("Using version %q for API group %q", CAPIVersion, CAPIGroup)

	p.gvr = schema.GroupVersionResource{
		Group:    CAPIGroup,
		Version:  CAPIVersion,
		Resource: resourceNameMachineDeployment,
	}
	dynamicClient, err := dynamic.NewForConfig(managementConfig)
	framework.ExpectNoError(err)

	discoveryClient := memory.NewMemCacheClient(p.managementClient.Discovery())
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient)
	managementScaleClient, err := scale.NewForConfig(
		managementConfig,
		mapper,
		dynamic.LegacyAPIPathResolverFunc,
		scale.NewDiscoveryScaleKindResolver(discoveryClient))
	framework.ExpectNoError(err)

	ns, err := p.managementClient.CoreV1().Namespaces().Create(context.TODO(), namespace, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	p.managementScaleClient = managementScaleClient
	p.machineDeploymentClient = dynamicClient.Resource(p.gvr).Namespace(*capiManagementNamespace)
	p.namespace = ns
}

func (p *Provider) FrameworkAfterEach(f *framework.Framework) {
	err := p.managementClient.CoreV1().Namespaces().Delete(context.TODO(), p.namespace.Name, metav1.DeleteOptions{})
	framework.ExpectNoError(err)
}

func (p *Provider) ResizeGroup(group string, size int32) error {
	panic("not implemented") // TODO: Implement
}

func (p *Provider) GetGroupNodes(group string) ([]string, error) {
	panic("not implemented") // TODO: Implement
}

func (p *Provider) GroupSize(group string) (int, error) {
	unstructuredResource, err := p.machineDeploymentClient.Get(context.TODO(), group, metav1.GetOptions{})
	if err != nil {
		return -1, err
	}

	scalableResource, err := p.managementScaleClient.Scales(unstructuredResource.GetNamespace()).
		Get(context.TODO(), p.gvr.GroupResource(), unstructuredResource.GetName(), metav1.GetOptions{})
	if err != nil {
		return -1, err
	}

	return int(scalableResource.Spec.Replicas), nil
}

func (p *Provider) DeleteNode(node *v1.Node) error {
	panic("not implemented") // TODO: Implement
}

func (p *Provider) CreatePD(zone string) (string, error) {
	panic("not implemented") // TODO: Implement
}

func (p *Provider) DeletePD(pdName string) error {
	panic("not implemented") // TODO: Implement
}

func (p *Provider) CreatePVSource(zone string, diskName string) (*v1.PersistentVolumeSource, error) {
	panic("not implemented") // TODO: Implement
}

func (p *Provider) DeletePVSource(pvSource *v1.PersistentVolumeSource) error {
	panic("not implemented") // TODO: Implement
}

func (p *Provider) CleanupServiceResources(c kubernetes.Interface, loadBalancerName string, region string, zone string) {
	panic("not implemented") // TODO: Implement
}

func (p *Provider) EnsureLoadBalancerResourcesDeleted(ip string, portRange string) error {
	panic("not implemented") // TODO: Implement
}

func (p *Provider) LoadBalancerSrcRanges() []string {
	panic("not implemented") // TODO: Implement
}

func (p *Provider) EnableAndDisableInternalLB() (enable func(svc *v1.Service), disable func(svc *v1.Service)) {
	panic("not implemented") // TODO: Implement
}

func (p *Provider) EnableAutoscaler(nodeGroup string, minSize int, maxSize int) error {
	var err error
	deployment, err = p.managementClient.AppsV1().Deployments(p.namespace.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	serviceAccount, err = p.managementClient.CoreV1().ServiceAccounts(p.namespace.Name).Create(context.TODO(), serviceAccount, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	clusterRole, err = p.managementClient.RbacV1().ClusterRoles().Create(context.TODO(), clusterRole, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	fmt.Println("!!!!!!!!", auth.IsRBACEnabled(p.managementClient.RbacV1()))
	err = auth.BindClusterRoleInNamespace(p.managementClient.RbacV1(),
		clusterRole.Name, p.namespace.Name, rbacv1.Subject{
			Kind:      rbacv1.ServiceAccountKind,
			Namespace: p.namespace.Name,
			Name:      autoscalerName,
		})
	if err != nil {
		return err
	}

	return nil
}

func (p *Provider) DisableAutoscaler(nodeGroup string) error {
	if err := p.managementClient.RbacV1().ClusterRoles().Delete(context.TODO(), autoscalerName, metav1.DeleteOptions{}); err != nil {
		return err
	}
	// return p.managementClient.AppsV1().Deployments(*capiManagementNamespace).Delete(context.TODO(), autoscalerName, metav1.DeleteOptions{})
	return nil
}

func (p *Provider) WaitForReadyNodes(client kubernetes.Interface, timeout time.Duration) error {
	panic("not implemented") // TODO: Implement
}

func (p *Provider) ResetInstanceGroups() error {
	panic("not implemented") // TODO: Implement
}
