package clusterapi

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingapi "k8s.io/api/autoscaling/v1"
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
	e2edeployment "k8s.io/kubernetes/test/e2e/framework/deployment"
	"k8s.io/utils/pointer"
)

const autoscalerName = "cluster-autoscaler"

var (
	capiManagementKubeConfig = flag.String(fmt.Sprintf("%s-%s", "capi-management", clientcmd.RecommendedConfigPathFlag), "", "Path to kubeconfig containing embedded authinfo for CAPI management cluster.")
	capiWorkloadKubeConfig   = flag.String(fmt.Sprintf("%s-%s", "capi-workload", clientcmd.RecommendedConfigPathFlag), "", "Path to kubeconfig containing embedded authinfo for CAPI workload cluster.")
	capiManagementNamespace  = flag.String("capi-management-namespace", "default", "Namespace in which the scalable resources are located")
	clusterAutoscalerImage   = flag.String("cluster-autoscaler-image", "", "Image to be used for the cluster autoscaler")
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
	clusterRole             *rbacv1.ClusterRole
	clusterRoleBinding      *rbacv1.ClusterRoleBinding
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

	namespace := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: autoscalerName + "-",
		},
	}
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
	unstructuredResource, err := p.machineDeploymentClient.Get(context.TODO(), group, metav1.GetOptions{})
	if err != nil {
		return err
	}

	_, err = p.managementScaleClient.Scales(unstructuredResource.GetNamespace()).
		Update(context.TODO(), p.gvr.GroupResource(), &autoscalingapi.Scale{
			Spec: autoscalingapi.ScaleSpec{Replicas: size},
			ObjectMeta: metav1.ObjectMeta{
				Name: unstructuredResource.GetName(),
			},
		}, metav1.UpdateOptions{})

	return err
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

func (p *Provider) EnableAutoscaler(nodeGroup string, minSize int, maxSize int) error {
	deployment := &appsv1.Deployment{
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
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": autoscalerName,
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:    autoscalerName,
							Image:   *clusterAutoscalerImage,
							Command: []string{"/cluster-autoscaler"},
							Args: []string{
								"--cloud-provider=clusterapi",
								"--kubeconfig=/home/workload/kubeconfig.yml",
								"--clusterapi-cloud-config-authoritative",
								"--scale-down-delay-after-add=30s",
								"--scale-down-delay-after-failure=30s",
								"--scale-down-unneeded-time=1m",
								"--scale-down-unready-time=30s",
								"--scan-interval=1s",
							},
							VolumeMounts: []v1.VolumeMount{
								{
									MountPath: "/home/workload",
									Name:      "workload-kubeconfig",
								},
							},
						},
					},
					Tolerations: []v1.Toleration{
						{
							Key:    "node-role.kubernetes.io/master",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
					ServiceAccountName: autoscalerName,
					Volumes: []v1.Volume{
						{
							Name: "workload-kubeconfig",
							VolumeSource: v1.VolumeSource{
								Secret: &v1.SecretVolumeSource{
									SecretName: autoscalerName,
								},
							},
						},
					},
				},
			},
		},
	}
	serviceAccount := &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: autoscalerName,
		},
	}
	p.clusterRole = &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: autoscalerName + "-",
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{rbacv1.APIGroupAll},
				Verbs:     []string{rbacv1.VerbAll},
				Resources: []string{rbacv1.ResourceAll},
			},
		},
	}
	workloadKubeconfigBytes, err := ioutil.ReadFile(*capiWorkloadKubeConfig)
	if err != nil {
		return err
	}

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: autoscalerName,
		},
		Data: map[string][]byte{
			"kubeconfig.yml": workloadKubeconfigBytes,
		},
	}

	_, err = p.managementClient.CoreV1().Secrets(p.namespace.Name).Create(context.TODO(), secret, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("err creating secret: %w", err)
	}

	_, err = p.managementClient.CoreV1().ServiceAccounts(p.namespace.Name).Create(context.TODO(), serviceAccount, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("err creating serviceaccount: %w", err)
	}

	clusterRole, err := p.managementClient.RbacV1().ClusterRoles().Create(context.TODO(), p.clusterRole, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("err creating clusterole: %w", err)
	}
	p.clusterRole = clusterRole

	p.clusterRoleBinding = &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: autoscalerName + "-",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     clusterRole.Name,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      autoscalerName,
				Namespace: p.namespace.Name,
			},
		},
	}
	clusterRoleBinding, err := p.managementClient.RbacV1().ClusterRoleBindings().Create(context.TODO(), p.clusterRoleBinding, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("err creating clusterrolebinding: %w", err)
	}
	p.clusterRoleBinding = clusterRoleBinding

	deployment, err = p.managementClient.AppsV1().Deployments(p.namespace.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("err creating deployment: %w", err)
	}

	err = e2edeployment.WaitForDeploymentComplete(p.managementClient, deployment)
	if err != nil {
		return fmt.Errorf("err waiting for deployment to complete: %w", err)
	}

	return nil
}

func (p *Provider) DisableAutoscaler(nodeGroup string) error {
	if err := p.managementClient.RbacV1().ClusterRoleBindings().Delete(context.TODO(), p.clusterRoleBinding.Name, metav1.DeleteOptions{}); err != nil {
		return err
	}
	return p.managementClient.RbacV1().ClusterRoles().Delete(context.TODO(), p.clusterRole.Name, metav1.DeleteOptions{})
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
