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

package node

import (
	"context"
	"fmt"
	"sync"
	"time"

	"knative.dev/pkg/logging"

	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
)

const (
	nodeName         = "node_name"
	nodeProvisioner  = "provisioner"
	nodeZone         = "zone"
	nodeArchitecture = "arch"
	nodeCapacityType = "capacity_type"
	nodeInstanceType = "instance_type"
	nodePhase        = "phase"
)

var (
	nodeCreatedHistVec = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "karpenter",
			Subsystem: "nodes",
			Name:      "created",
			Help:      "Node resource creation",
			Buckets:   prometheus.LinearBuckets(10, 5, 50),
		},
		labelNames(),
	)
)

func init() {
	crmetrics.Registry.MustRegister(nodeCreatedHistVec)
}

func labelNames() []string {
	return []string{
		nodeName,
		nodeProvisioner,
		nodeZone,
		nodeArchitecture,
		nodeCapacityType,
		nodeInstanceType,
		nodePhase,
	}
}

type Controller struct {
	kubeClient      client.Client
	labelCollection sync.Map
}

// NewController constructs a controller instance
func NewController(kubeClient client.Client) *Controller {
	return &Controller{
		kubeClient: kubeClient,
	}
}

// Reconcile executes a termination control loop for the resource
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	ctx = logging.WithLogger(ctx, logging.FromContext(ctx).Named("canarymetrics").With("node", req.Name))
	// Remove the previous gauge after node labels are updated
	//c.cleanup(req.NamespacedName)
	// Retrieve node from reconcile request
	node := &v1.Node{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, node); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	if err := c.record(ctx, node); err != nil {
		logging.FromContext(ctx).Errorf("Failed to update histograms: %s", err)
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Register(ctx context.Context, m manager.Manager) error {
	return controllerruntime.
		NewControllerManagedBy(m).
		Named("canarymetrics").
		For(&v1.Node{}).
		Complete(c)
}

func (c *Controller) cleanup(nodeNamespacedName types.NamespacedName) {
	if labelSet, ok := c.labelCollection.Load(nodeNamespacedName); ok {
		for _, labels := range labelSet.([]prometheus.Labels) {
			nodeCreatedHistVec.Delete(labels)
		}
	}
	c.labelCollection.Store(nodeNamespacedName, []prometheus.Labels{})
}

func (c *Controller) record(ctx context.Context, node *v1.Node) error {
	// Populate  metrics
	if err := c.set(node); err != nil {
		logging.FromContext(ctx).Errorf("Failed to generate histogram: %s", err)
	}
	return nil
}

func (c *Controller) labels(node *v1.Node) prometheus.Labels {
	metricLabels := prometheus.Labels{}
	metricLabels[nodeName] = node.GetName()
	if provisionerName, ok := node.Labels[v1alpha5.ProvisionerNameLabelKey]; !ok {
		metricLabels[nodeProvisioner] = "N/A"
	} else {
		metricLabels[nodeProvisioner] = provisionerName
	}
	metricLabels[nodeZone] = node.Labels[v1.LabelTopologyZone]
	metricLabels[nodeArchitecture] = node.Labels[v1.LabelArchStable]
	if capacityType, ok := node.Labels[v1alpha5.LabelCapacityType]; !ok {
		metricLabels[nodeCapacityType] = "N/A"
	} else {
		metricLabels[nodeCapacityType] = capacityType
	}
	metricLabels[nodeInstanceType] = node.Labels[v1.LabelInstanceTypeStable]
	metricLabels[nodePhase] = string(node.Status.Phase)
	return metricLabels
}

func (c *Controller) set(node *v1.Node) error {
	labels := c.labels(node)
	nodeNamespacedName := types.NamespacedName{Name: node.Name}
	existingLabels, _ := c.labelCollection.LoadOrStore(nodeNamespacedName, []prometheus.Labels{})
	existingLabels = append(existingLabels.([]prometheus.Labels), labels)
	c.labelCollection.Store(nodeNamespacedName, existingLabels)

	createdHistogram, err := nodeCreatedHistVec.GetMetricWith(labels)
	if err != nil {
		return fmt.Errorf("generate new histogram: %w", err)
	}
	for _, cond := range node.Status.Conditions {
		if cond.Reason == "KubeletReady" && cond.Status == v1.ConditionTrue {
			// Record the duration between node resource creation and kubelet Ready
			createdHistogram.Observe(time.Duration(cond.LastTransitionTime.UnixMilli() - node.CreationTimestamp.UnixMilli()).Seconds())
		}
	}
	return nil
}
