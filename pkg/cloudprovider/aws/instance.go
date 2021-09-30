/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/avast/retry-go"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/awslabs/karpenter/pkg/apis/provisioning/v1alpha4"
	"github.com/awslabs/karpenter/pkg/cloudprovider"
	v1alpha1 "github.com/awslabs/karpenter/pkg/cloudprovider/aws/apis/v1alpha1"
	"github.com/awslabs/karpenter/pkg/utils/functional"
	"knative.dev/pkg/logging"

	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	EC2InstanceIDNotFoundErrCode = "InvalidInstanceID.NotFound"
)

type InstanceProvider struct {
	ec2api               ec2iface.EC2API
	instanceTypeProvider *InstanceTypeProvider
}

// Create an instance given the constraints.
// instanceTypes should be sorted by priority for spot capacity type.
// If spot is not used, the instanceTypes are not required to be sorted
// because we are using ec2 fleet's lowest-price OD allocation strategy
func (p *InstanceProvider) Create(ctx context.Context,
	launchTemplate string,
	instanceTypes []cloudprovider.InstanceType,
	subnets []*ec2.Subnet,
	capacityTypes []string,
	quantity int,
) ([]*v1.Node, error) {
	// 1. Launch Instance
	ids, err := p.launchInstances(ctx, launchTemplate, instanceTypes, subnets, capacityTypes, quantity)
	if err != nil {
		return nil, err
	}
	// 2. Get Instance with backoff retry since EC2 is eventually consistent
	instances := []*ec2.Instance{}
	if err := retry.Do(
		func() (err error) { instances, err = p.getInstances(ctx, ids); return err },
		retry.Delay(1*time.Second),
		retry.Attempts(3),
	); err != nil {
		return nil, err
	}
	for _, instance := range instances {
		logging.FromContext(ctx).Infof("Launched instance: %s, type: %s, zone: %s, hostname: %s",
			aws.StringValue(instance.InstanceId),
			aws.StringValue(instance.InstanceType),
			aws.StringValue(instance.Placement.AvailabilityZone),
			aws.StringValue(instance.PrivateDnsName),
		)
	}

	// 3. Convert Instance(s) to Node(s)
	nodes := []*v1.Node{}
	for _, instance := range instances {
		node, err := p.instanceToNode(ctx, instance, instanceTypes)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

func (p *InstanceProvider) Terminate(ctx context.Context, node *v1.Node) error {
	id, err := getInstanceID(node)
	if err != nil {
		return fmt.Errorf("getting instance ID for node %s, %w", node.Name, err)
	}
	if _, err = p.ec2api.TerminateInstancesWithContext(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []*string{id},
	}); err != nil {
		if aerr, ok := err.(awserr.Error); ok && aerr.Code() == EC2InstanceIDNotFoundErrCode {
			return nil
		}
		return fmt.Errorf("terminating instance %s, %w", node.Name, err)
	}
	return nil
}

func (p *InstanceProvider) launchInstances(ctx context.Context,
	launchTemplateName string,
	instanceTypeOptions []cloudprovider.InstanceType,
	subnets []*ec2.Subnet,
	capacityTypes []string,
	quantity int) ([]*string, error) {

	// If unconstrained, default to spot to save on cost, otherwise use on
	// demand. Constraint solving logic will guarantee that capacityTypes are
	// either unconstrained (nil), or a valid WellKnownLabel. Provisioner
	// defaulting logic will currently default to [on-demand] if unspecifed.
	capacityType := v1alpha1.CapacityTypeSpot
	if capacityTypes != nil && !functional.ContainsString(capacityTypes, v1alpha1.CapacityTypeSpot) {
		capacityType = v1alpha1.CapacityTypeOnDemand
	}

	// 1. Construct override options.
	overrides := p.getOverrides(instanceTypeOptions, subnets, capacityType)
	if len(overrides) == 0 {
		return nil, fmt.Errorf("no viable {subnet, instanceType} combination")
	}

	// 2. Create fleet
	createFleetOutput, err := p.ec2api.CreateFleetWithContext(ctx, &ec2.CreateFleetInput{
		Type: aws.String(ec2.FleetTypeInstant),
		TargetCapacitySpecification: &ec2.TargetCapacitySpecificationRequest{
			DefaultTargetCapacityType: aws.String(capacityType),
			TotalTargetCapacity:       aws.Int64(int64(quantity)),
		},
		// OnDemandOptions are allowed to be specified even when requesting spot
		OnDemandOptions: &ec2.OnDemandOptionsRequest{
			AllocationStrategy: aws.String(ec2.FleetOnDemandAllocationStrategyLowestPrice),
		},
		// SpotOptions are allowed to be specified even when requesting on-demand
		SpotOptions: &ec2.SpotOptionsRequest{
			AllocationStrategy: aws.String(ec2.SpotAllocationStrategyCapacityOptimizedPrioritized),
		},
		LaunchTemplateConfigs: []*ec2.FleetLaunchTemplateConfigRequest{{
			LaunchTemplateSpecification: &ec2.FleetLaunchTemplateSpecificationRequest{
				LaunchTemplateName: aws.String(launchTemplateName),
				Version:            aws.String("$Default"),
			},
			Overrides: overrides,
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("creating fleet %w", err)
	}
	instanceIds := combineFleetInstances(*createFleetOutput)
	if len(instanceIds) != quantity {
		return nil, combineFleetErrors(createFleetOutput.Errors)
	}
	return instanceIds, nil
}

func (p *InstanceProvider) getOverrides(instanceTypeOptions []cloudprovider.InstanceType, subnets []*ec2.Subnet, capacityType string) []*ec2.FleetLaunchTemplateOverridesRequest {
	var overrides []*ec2.FleetLaunchTemplateOverridesRequest
	for i, instanceType := range instanceTypeOptions {
		for _, zone := range instanceType.Zones() {
			for _, subnet := range subnets {
				if aws.StringValue(subnet.AvailabilityZone) == zone {
					override := &ec2.FleetLaunchTemplateOverridesRequest{
						InstanceType: aws.String(instanceType.Name()),
						SubnetId:     subnet.SubnetId,
					}
					// Add a priority for spot requests since we are using the capacity-optimized-prioritized spot allocation strategy
					// to reduce the likelihood of getting an excessively large instance type.
					// instanceTypeOptions are sorted by vcpus and memory so this prioritizes smaller instance types.
					if capacityType == v1alpha1.CapacityTypeSpot {
						override.Priority = aws.Float64(float64(i))
					}
					overrides = append(overrides, override)
					// FleetAPI cannot span subnets from the same AZ, so break after the first one.
					break
				}
			}
		}
	}
	return overrides
}

func (p *InstanceProvider) getInstances(ctx context.Context, ids []*string) ([]*ec2.Instance, error) {
	describeInstancesOutput, err := p.ec2api.DescribeInstancesWithContext(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
	if aerr, ok := err.(awserr.Error); ok && aerr.Code() == EC2InstanceIDNotFoundErrCode {
		return nil, aerr
	}
	if err != nil {
		return nil, fmt.Errorf("failed to describe ec2 instances, %w", err)
	}
	describedInstances := combineReservations(describeInstancesOutput.Reservations)
	if len(describedInstances) != len(ids) {
		return nil, fmt.Errorf("expected a %d instance, got %d", len(ids), len(describedInstances))
	}
	instances := []*ec2.Instance{}
	for _, instance := range describedInstances {
		if len(aws.StringValue(instance.PrivateDnsName)) == 0 {
			return nil, fmt.Errorf("got instance %s but PrivateDnsName was not set", aws.StringValue(instance.InstanceId))
		}
		instances = append(instances, instance)
	}
	return instances, nil
}

func (p *InstanceProvider) instanceToNode(ctx context.Context, instance *ec2.Instance, instanceTypes []cloudprovider.InstanceType) (*v1.Node, error) {
	for _, instanceType := range instanceTypes {
		if instanceType.Name() == aws.StringValue(instance.InstanceType) {
			return &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: aws.StringValue(instance.PrivateDnsName),
				},
				Spec: v1.NodeSpec{
					ProviderID: fmt.Sprintf("aws:///%s/%s", aws.StringValue(instance.Placement.AvailabilityZone), aws.StringValue(instance.InstanceId)),
				},
				Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
						v1.ResourcePods:   *instanceType.Pods(),
						v1.ResourceCPU:    *instanceType.CPU(),
						v1.ResourceMemory: *instanceType.Memory(),
					},
					NodeInfo: v1.NodeSystemInfo{
						Architecture:    aws.StringValue(instance.Architecture),
						OperatingSystem: v1alpha4.OperatingSystemLinux,
					},
				},
			}, nil
		}
	}
	return nil, fmt.Errorf("unrecognized instance type %s", aws.StringValue(instance.InstanceType))
}

func getInstanceID(node *v1.Node) (*string, error) {
	id := strings.Split(node.Spec.ProviderID, "/")
	if len(id) < 5 {
		return nil, fmt.Errorf("parsing instance id %s", node.Spec.ProviderID)
	}
	return aws.String(id[4]), nil
}

func combineFleetErrors(errors []*ec2.CreateFleetError) (errs error) {
	unique := sets.NewString()
	for _, err := range errors {
		unique.Insert(fmt.Sprintf("%s: %s", aws.StringValue(err.ErrorCode), aws.StringValue(err.ErrorMessage)))
	}
	for _, errorCode := range unique.List() {
		errs = multierr.Append(errs, fmt.Errorf(errorCode))
	}
	return fmt.Errorf("with fleet error(s), %w", errs)
}

func combineFleetInstances(createFleetOutput ec2.CreateFleetOutput) []*string {
	instanceIds := []*string{}
	for _, reservation := range createFleetOutput.Instances {
		instanceIds = append(instanceIds, reservation.InstanceIds...)
	}
	return instanceIds
}

func combineReservations(reservations []*ec2.Reservation) []*ec2.Instance {
	instances := []*ec2.Instance{}
	for _, reservation := range reservations {
		instances = append(instances, reservation.Instances...)
	}
	return instances
}
