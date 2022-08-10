package integration_test

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/karpenter/pkg/apis/awsnodetemplate/v1alpha1"
	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	awsv1alpha1 "github.com/aws/karpenter/pkg/cloudprovider/aws/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/test"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo"
)

// This test requires the EBS CSI driver to be installed
var _ = Describe("Dynamic PVC", func() {
	BeforeEach(func() { env.BeforeEach() })
	AfterEach(func() { env.AfterEach() })

	It("should run a pod with a dynamic persistent volume", func() {
		// Ensure that the EBS driver is installed, or we can't run the test.
		var ds appsv1.DaemonSet
		if err := env.Client.Get(env.Context, client.ObjectKey{
			Namespace: "kube-system",
			Name:      "ebs-csi-node",
		}, &ds); err != nil {
			if errors.IsNotFound(err) {
				Skip(fmt.Sprintf("skipping dynamic PVC test due to missing EBS driver %s", err))
			} else {
				Fail(fmt.Sprintf("determining EBS driver status, %s", err))
			}
		}

		provider := test.AWSNodeTemplate(v1alpha1.AWSNodeTemplateSpec{AWS: awsv1alpha1.AWS{
			SecurityGroupSelector: map[string]string{"karpenter.sh/discovery": env.ClusterName},
			SubnetSelector:        map[string]string{"karpenter.sh/discovery": env.ClusterName},
		}})
		provisioner := test.Provisioner(test.ProvisionerOptions{
			ProviderRef: &v1alpha5.ProviderRef{Name: provider.Name}})

		storageClassName := "ebs-sc-test"
		bindMode := storagev1.VolumeBindingWaitForFirstConsumer
		sc := test.StorageClass(test.StorageClassOptions{
			ObjectMeta: metav1.ObjectMeta{
				Name: storageClassName,
			},
			Provisioner:       aws.String("ebs.csi.aws.com"),
			VolumeBindingMode: &bindMode,
		})

		pvc := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
			ObjectMeta: metav1.ObjectMeta{
				Name: "ebs-claim",
			},
			StorageClassName: aws.String(storageClassName),
			Resources:        v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("5Gi")}},
		})

		pod := test.Pod(test.PodOptions{
			PersistentVolumeClaims: []string{pvc.Name},
		})

		env.ExpectCreated(provisioner, provider, sc, pvc, pod)
		env.EventuallyExpectHealthy(pod)
		env.ExpectCreatedNodeCount("==", 1)
		env.ExpectDeleted(pod)
	})
})

var _ = Describe("Static PVC", func() {
	It("should run a pod with a static persistent volume", func() {
		provider := test.AWSNodeTemplate(v1alpha1.AWSNodeTemplateSpec{AWS: awsv1alpha1.AWS{
			SecurityGroupSelector: map[string]string{"karpenter.sh/discovery": env.ClusterName},
			SubnetSelector:        map[string]string{"karpenter.sh/discovery": env.ClusterName},
		}})
		provisioner := test.Provisioner(test.ProvisionerOptions{
			ProviderRef: &v1alpha5.ProviderRef{Name: provider.Name}})

		storageClassName := "nfs-test"
		bindMode := storagev1.VolumeBindingWaitForFirstConsumer
		sc := test.StorageClass(test.StorageClassOptions{
			ObjectMeta: metav1.ObjectMeta{
				Name: storageClassName,
			},
			VolumeBindingMode: &bindMode,
		})

		pv := test.PersistentVolume(test.PersistentVolumeOptions{
			ObjectMeta:       metav1.ObjectMeta{Name: "nfs-test-volume"},
			StorageClassName: "nfs-test",
		})

		// the server here doesn't need to actually exist for the pod to start running
		pv.Spec.NFS = &v1.NFSVolumeSource{
			Server: "fake.server",
			Path:   "/some/path",
		}
		pv.Spec.CSI = nil

		pvc := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
			ObjectMeta: metav1.ObjectMeta{
				Name: "nfs-claim",
			},
			StorageClassName: aws.String(storageClassName),
			VolumeName:       pv.Name,
			Resources:        v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("5Gi")}},
		})

		pod := test.Pod(test.PodOptions{
			PersistentVolumeClaims: []string{pvc.Name},
		})

		env.ExpectCreated(provisioner, provider, sc, pv, pvc, pod)
		env.EventuallyExpectHealthy(pod)
		env.ExpectCreatedNodeCount("==", 1)
		env.ExpectDeleted(pod)
	})
})
