package integration_test

import (
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/karpenter/pkg/apis/awsnodetemplate/v1alpha1"
	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	awsv1alpha1 "github.com/aws/karpenter/pkg/cloudprovider/aws/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/test"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
)

var _ = Describe("Tags", func() {
	BeforeEach(func() {

	})

	It("should tag all associated resources", func() {
		provider := test.AWSNodeTemplate(v1alpha1.AWSNodeTemplateSpec{
			AWS: awsv1alpha1.AWS{
				SecurityGroupSelector: map[string]string{"karpenter.sh/discovery": env.ClusterName},
				SubnetSelector:        map[string]string{"karpenter.sh/discovery": env.ClusterName},
				Tags:                  map[string]string{"TestTag": "TestVal"},
			},
		})
		provisioner := test.Provisioner(test.ProvisionerOptions{ProviderRef: &v1alpha5.ProviderRef{Name: provider.Name}})
		pod := test.Pod()

		env.ExpectCreated(pod, provider, provisioner)
		env.EventuallyExpectHealthy(pod)
		env.ExpectCreatedNodeCount("==", 1)
		instance := env.GetInstance(pod.Spec.NodeName)
		volumeTags := tagMap(env.GetVolume(instance.BlockDeviceMappings[0].Ebs.VolumeId).Tags)
		instanceTags := tagMap(instance.Tags)

		Expect(instanceTags).To(ContainElement("TestVal"))
		Expect(volumeTags).To(ContainElement("TestVal"))
	})
})

func tagMap(tags []*ec2.Tag) map[string]string {
	return lo.SliceToMap(tags, func(tag *ec2.Tag) (string, string) {
		return *tag.Key, *tag.Value
	})
}
