package integration_test

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/karpenter/pkg/apis/awsnodetemplate/v1alpha1"
	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	awsv1alpha1 "github.com/aws/karpenter/pkg/cloudprovider/aws/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/test"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("InstanceProfile", func() {
	var roleName string

	BeforeEach(func() {
		roleName = fmt.Sprintf("KarpenterIntegTestInstanceProfile-%s", env.Region)
	})

	AfterEach(func() {
		deleteNodeInstanceProfile(roleName)
	})

	FIt("should use instance profile from the aws node template", func() {
		provider := test.AWSNodeTemplate(v1alpha1.AWSNodeTemplateSpec{
			AWS: awsv1alpha1.AWS{
				SecurityGroupSelector: map[string]string{"karpenter.sh/discovery": env.ClusterName},
				SubnetSelector:        map[string]string{"karpenter.sh/discovery": env.ClusterName},
				InstanceProfile:       aws.String(createNodeInstanceProfile(roleName)),
			},
		})

		provisioner := test.Provisioner(test.ProvisionerOptions{ProviderRef: &v1alpha5.ProviderRef{Name: provider.Name}})
		pod := test.Pod()

		env.ExpectCreated(pod, provider, provisioner)
		env.EventuallyExpectHealthy(pod)
		env.ExpectCreatedNodeCount("==", 1)
		env.ExpectInstance(pod.Spec.NodeName).To(HaveField("IamInstanceProfile", HaveValue(ContainElement(ContainSubstring(roleName)))))
	})

})

func createNodeInstanceProfile(roleName string) string {
	createRoleInput := &iam.CreateRoleInput{
		RoleName: aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(`{
			"Version": "2012-10-17",
			"Statement": [
				{
					"Effect": "Allow",
					"Principal": {
						"Service": "ec2.amazonaws.com"
					},
					"Action": "sts:AssumeRole"
				}
			]
		}`),
	}
	_, err := env.IAMAPI.CreateRole(createRoleInput)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok && aerr.Code() == iam.ErrCodeEntityAlreadyExistsException {
			deleteNodeInstanceProfile(roleName)
			_, err = env.IAMAPI.CreateRole(createRoleInput)
		}
		Expect(err).ToNot(HaveOccurred())
	}
	_, err = env.IAMAPI.CreateInstanceProfile(&iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(roleName),
	})
	Expect(err).ToNot(HaveOccurred())
	_, err = env.IAMAPI.AddRoleToInstanceProfile(&iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(roleName),
		RoleName:            aws.String(roleName),
	})
	Expect(err).ToNot(HaveOccurred())
	managedPolicies := []string{
		"arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
		"arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
		"arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
		"arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore",
	}
	for _, policy := range managedPolicies {
		_, err = env.IAMAPI.AttachRolePolicy(&iam.AttachRolePolicyInput{
			RoleName:  aws.String(roleName),
			PolicyArn: aws.String(policy),
		})
		Expect(err).ToNot(HaveOccurred())
	}
	return roleName
}

func deleteNodeInstanceProfile(roleName string) {
	_, err := env.IAMAPI.RemoveRoleFromInstanceProfile(&iam.RemoveRoleFromInstanceProfileInput{
		InstanceProfileName: aws.String(roleName),
		RoleName:            aws.String(roleName),
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); !ok || aerr.Code() != iam.ErrCodeNoSuchEntityException {
			Expect(err).ToNot(HaveOccurred())
		}
	}
	_, err = env.IAMAPI.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String(roleName)})
	if err != nil {
		if aerr, ok := err.(awserr.Error); !ok || aerr.Code() != iam.ErrCodeNoSuchEntityException {
			Expect(err).ToNot(HaveOccurred())
		}
	}
	err = env.IAMAPI.ListAttachedRolePoliciesPages(&iam.ListAttachedRolePoliciesInput{RoleName: aws.String(roleName)}, func(lrpo *iam.ListAttachedRolePoliciesOutput, _ bool) bool {
		for _, policy := range lrpo.AttachedPolicies {
			_, err = env.IAMAPI.DetachRolePolicy(&iam.DetachRolePolicyInput{RoleName: aws.String(roleName), PolicyArn: policy.PolicyArn})
			Expect(err).ToNot(HaveOccurred())
		}
		return true
	})
	Expect(err).ToNot(HaveOccurred())
	_, err = env.IAMAPI.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(roleName)})
	Expect(err).ToNot(HaveOccurred())
}
