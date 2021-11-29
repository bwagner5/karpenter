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

package provisioning

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/controllers/provisioning/scheduling"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	MaxBatchDuration = time.Second * 15
	MinBatchDuration = time.Second * 3
	// MaxPodsPerBatch limits the number of pods we process at one time to avoid using too much memory
	MaxPodsPerBatch = 10_000
)

// Provisioner waits for enqueued pods, batches them, creates capacity and binds the pods to the capacity.
type Provisioner struct {
	// State
	*v1alpha5.Provisioner
	pods    chan *v1.Pod
	results chan error
	done    <-chan struct{}
	Stop    context.CancelFunc
	// Dependencies
	cloudProvider cloudprovider.CloudProvider
	kubeClient    client.Client
	scheduler     *scheduling.Scheduler
	launcher      *Launcher
}

func (p *Provisioner) Start(ctx context.Context) {
	go func() {
		logging.FromContext(ctx).Info("Starting provisioner")
		for {
			select {
			case <-p.done:
				logging.FromContext(ctx).Info("Stopping provisioner")
				return
			default:
				if err := p.provision(ctx); err != nil {
					logging.FromContext(ctx).Errorf("Provisioning failed, %s", err.Error())
				}
			}
		}
	}()
}

func (p *Provisioner) provision(ctx context.Context) (err error) {
	// Wait for a batch of pods
	pods := p.Batch(ctx)
	// Communicate the result of the provisioning loop to each of the pods.
	defer func() {
		for i := 0; i < len(pods); i++ {
			select {
			case p.results <- err: // Block until result is communicated
			case <-p.done: // Leave if closed
			}
		}
	}()
	// Separate pods by scheduling constraints
	schedules, err := p.scheduler.Solve(ctx, p.Provisioner, pods)
	if err != nil {
		return fmt.Errorf("solving scheduling constraints, %w", err)
	}
	// Get Instance Types, offering availability may vary over time
	instanceTypes, err := p.cloudProvider.GetInstanceTypes(ctx, &p.Spec.Constraints)
	if err != nil {
		return fmt.Errorf("getting instance types")
	}
	// Launch capacity and bind pods
	if err := p.launcher.Launch(ctx, p.Provisioner, schedules, instanceTypes); err != nil {
		return fmt.Errorf("launching capacity, %w", err)
	}
	return nil
}

// Add a pod to the provisioner and block until it's processed. The caller
// is responsible for verifying that the pod was scheduled correctly. In the
// future, this may be expanded to include concepts such as retriable errors.
func (p *Provisioner) Add(ctx context.Context, pod *v1.Pod) (err error) {
	select {
	case p.pods <- pod: // Block until pod is enqueued
	case <-p.done: // Leave if closed
	}
	select {
	case err = <-p.results: // Block until result is sent
	case <-p.done: // Leave if closed
	}
	return err
}

// Batch returns a slice of enqueued pods after idle or timeout
func (p *Provisioner) Batch(ctx context.Context) (pods []*v1.Pod) {
	logging.FromContext(ctx).Infof("Waiting for unschedulable pods")
	// Start the batching window after the first pod is received
	pods = append(pods, <-p.pods)
	timeout := time.NewTimer(MaxBatchDuration)
	idle := time.NewTimer(MinBatchDuration)
	start := time.Now()
	defer func() {
		pods = p.FilterProvisionable(ctx, pods)
		logging.FromContext(ctx).Infof("Batched %d pods in %s", len(pods), time.Since(start))
	}()
	for {
		select {
		case pod := <-p.pods:
			idle.Reset(MinBatchDuration)
			pods = append(pods, pod)
			if len(pods) >= MaxPodsPerBatch {
				return pods
			}
		case <-ctx.Done():
			return pods
		case <-timeout.C:
			return pods
		case <-idle.C:
			return pods
		}
	}
}

// FilterProvisionable removes pods that have been assigned a node.
// This check is needed to prevent duplicate binds when a pod is scheduled to a node
// between the time it was ingested into the scheduler and the time it is included
// in a provisioner batch.
func (p *Provisioner) FilterProvisionable(ctx context.Context, pods []*v1.Pod) []*v1.Pod {
	provisionable := []*v1.Pod{}
	for _, pod := range pods {
		// the original pod should be returned rather than the newly fetched pod in case the scheduler relaxed constraints
		original := pod
		candidate := &v1.Pod{}
		if err := p.kubeClient.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, candidate); err != nil {
			logging.FromContext(ctx).Errorf("Unexpected error retrieving pod \"%s/%s\" while checking if it is provisionable", pod.Namespace, pod.Name)
		}
		if candidate.Spec.NodeName == "" {
			provisionable = append(provisionable, original)
		}
	}
	return provisionable
}
