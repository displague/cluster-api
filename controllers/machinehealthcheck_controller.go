/*
Copyright 2020 The Kubernetes Authors.

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

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha3"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	machineNodeNameIndex = "status.nodeRef.name"

	// Event types

	// EventRemediationRestricted is emitted in case when machine remediation
	// is restricted by remediation circuit shorting logic
	EventRemediationRestricted string = "RemediationRestricted"
)

// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinehealthchecks;machinehealthchecks/status,verbs=get;list;watch;update;patch

// MachineHealthCheckReconciler reconciles a MachineHealthCheck object
type MachineHealthCheckReconciler struct {
	Client  client.Client
	Log     logr.Logger
	Tracker *remote.ClusterCacheTracker

	controller controller.Controller
	recorder   record.EventRecorder
	scheme     *runtime.Scheme
}

func (r *MachineHealthCheckReconciler) SetupWithManager(mgr ctrl.Manager, options controller.Options) error {
	controller, err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.MachineHealthCheck{}).
		Watches(
			&source.Kind{Type: &clusterv1.Machine{}},
			&handler.EnqueueRequestsFromMapFunc{ToRequests: handler.ToRequestsFunc(r.machineToMachineHealthCheck)},
		).
		WithOptions(options).
		WithEventFilter(predicates.ResourceNotPaused(r.Log)).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "failed setting up with a controller manager")
	}
	err = controller.Watch(
		&source.Kind{Type: &clusterv1.Cluster{}},
		&handler.EnqueueRequestsFromMapFunc{ToRequests: handler.ToRequestsFunc(r.clusterToMachineHealthCheck)},
		// TODO: should this wait for Cluster.Status.InfrastructureReady similar to Infra Machine resources?
		predicates.ClusterUnpaused(r.Log),
	)
	if err != nil {
		return errors.Wrap(err, "failed to add Watch for Clusters to controller manager")
	}

	// Add index to Machine for listing by Node reference
	if err := mgr.GetCache().IndexField(&clusterv1.Machine{},
		machineNodeNameIndex,
		r.indexMachineByNodeName,
	); err != nil {
		return errors.Wrap(err, "error setting index fields")
	}

	r.controller = controller
	r.recorder = mgr.GetEventRecorderFor("machinehealthcheck-controller")
	r.scheme = mgr.GetScheme()
	return nil
}

func (r *MachineHealthCheckReconciler) Reconcile(req ctrl.Request) (_ ctrl.Result, reterr error) {
	ctx := context.Background()
	logger := r.Log.WithValues("machinehealthcheck", req.Name, "namespace", req.Namespace)

	// Fetch the MachineHealthCheck instance
	m := &clusterv1.MachineHealthCheck{}
	if err := r.Client.Get(ctx, req.NamespacedName, m); err != nil {
		if apierrors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return ctrl.Result{}, nil
		}

		// Error reading the object - requeue the request.
		logger.Error(err, "Failed to fetch MachineHealthCheck")
		return ctrl.Result{}, err
	}

	cluster, err := util.GetClusterByName(ctx, r.Client, m.Namespace, m.Spec.ClusterName)
	if err != nil {
		logger.Error(err, "Failed to fetch Cluster for MachineHealthCheck")
		return ctrl.Result{}, errors.Wrapf(err, "failed to get Cluster %q for MachineHealthCheck %q in namespace %q",
			m.Spec.ClusterName, m.Name, m.Namespace)
	}

	logger = r.Log.WithValues("cluster", cluster.Name)

	// Return early if the object or Cluster is paused.
	if annotations.IsPaused(cluster, m) {
		logger.Info("Reconciliation is paused for this object")
		return ctrl.Result{}, nil
	}

	// Initialize the patch helper
	patchHelper, err := patch.NewHelper(m, r.Client)
	if err != nil {
		logger.Error(err, "Failed to build patch helper")
		return ctrl.Result{}, err
	}

	defer func() {
		// Always attempt to patch the object and status after each reconciliation.
		if err := patchHelper.Patch(ctx, m); err != nil {
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	// Reconcile labels.
	if m.Labels == nil {
		m.Labels = make(map[string]string)
	}
	m.Labels[clusterv1.ClusterLabelName] = m.Spec.ClusterName

	result, err := r.reconcile(ctx, logger, cluster, m)
	if err != nil {
		logger.Error(err, "Failed to reconcile MachineHealthCheck")
		r.recorder.Eventf(m, corev1.EventTypeWarning, "ReconcileError", "%v", err)

		// Requeue immediately if any errors occurred
		return ctrl.Result{}, err
	}

	return result, nil
}

func (r *MachineHealthCheckReconciler) reconcile(ctx context.Context, logger logr.Logger, cluster *clusterv1.Cluster, m *clusterv1.MachineHealthCheck) (ctrl.Result, error) {
	// Ensure the MachineHealthCheck is owned by the Cluster it belongs to
	m.OwnerReferences = util.EnsureOwnerRef(m.OwnerReferences, metav1.OwnerReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Cluster",
		Name:       cluster.Name,
		UID:        cluster.UID,
	})

	// Get the remote cluster cache to use as a client.Reader.
	remoteClient, err := r.Tracker.GetClient(ctx, util.ObjectKey(cluster))
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.watchClusterNodes(ctx, cluster); err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "Error watching nodes on target cluster")
	}

	// fetch all targets
	logger.V(3).Info("Finding targets")
	targets, err := r.getTargetsFromMHC(remoteClient, m)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "Failed to fetch targets from MachineHealthCheck")
	}
	totalTargets := len(targets)
	m.Status.ExpectedMachines = int32(totalTargets)
	m.Status.Targets = make([]string, totalTargets)
	for i, t := range targets {
		m.Status.Targets[i] = t.Machine.Name
	}

	// health check all targets and reconcile mhc status
	healthy, unhealthy, nextCheckTimes := r.healthCheckTargets(targets, logger, m.Spec.NodeStartupTimeout.Duration)
	m.Status.CurrentHealthy = int32(len(healthy))

	// check MHC current health against MaxUnhealthy
	if !isAllowedRemediation(m) {
		logger.V(3).Info(
			"Short-circuiting remediation",
			"total target", totalTargets,
			"max unhealthy", m.Spec.MaxUnhealthy,
			"unhealthy targets", len(unhealthy),
		)

		r.recorder.Eventf(
			m,
			corev1.EventTypeWarning,
			EventRemediationRestricted,
			"Remediation restricted due to exceeded number of unhealthy machines (total: %v, unhealthy: %v, maxUnhealthy: %v)",
			totalTargets,
			m.Status.CurrentHealthy,
			m.Spec.MaxUnhealthy,
		)
		for _, t := range append(healthy, unhealthy...) {
			if err := t.patchHelper.Patch(ctx, t.Machine); err != nil {
				return ctrl.Result{}, errors.Wrapf(err, "Failed to patch machine status for machine %q", t.Machine.Name)
			}
		}
		return reconcile.Result{Requeue: true}, nil
	}
	logger.V(3).Info(
		"Remediations are allowed",
		"total target", totalTargets,
		"max unhealthy", m.Spec.MaxUnhealthy,
		"unhealthy targets", len(unhealthy),
	)

	// mark for remediation
	errList := []error{}
	for _, t := range unhealthy {
		logger.V(3).Info("Target meets unhealthy criteria, triggers remediation", "target", t.string())

		conditions.MarkFalse(t.Machine, clusterv1.MachineOwnerRemediatedCondition, clusterv1.WaitingForRemediation, clusterv1.ConditionSeverityWarning, "MachineHealthCheck failed")
		if err := t.patchHelper.Patch(ctx, t.Machine); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "Failed to patch unhealthy machine status for machine %q", t.Machine.Name)
		}
		r.recorder.Eventf(
			t.Machine,
			corev1.EventTypeNormal,
			EventMachineMarkedUnhealthy,
			"Machine %v has been marked as unhealthy",
			t.string(),
		)
	}
	for _, t := range healthy {
		logger.V(3).Info("patching machine", "machine", t.Machine.GetName())
		if err := t.patchHelper.Patch(ctx, t.Machine); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "Failed to patch healthy machine status for machine %q", t.Machine.Name)
		}
	}

	// handle update errors
	if len(errList) > 0 {
		logger.V(3).Info("Error(s) marking machine, requeueing")
		return reconcile.Result{}, kerrors.NewAggregate(errList)
	}

	if minNextCheck := minDuration(nextCheckTimes); minNextCheck > 0 {
		logger.V(3).Info("Some targets might go unhealthy. Ensuring a requeue happens", "requeueIn", minNextCheck.Truncate(time.Second).String())
		return ctrl.Result{RequeueAfter: minNextCheck}, nil
	}

	logger.V(3).Info("No more targets meet unhealthy criteria")

	return ctrl.Result{}, nil
}

// clusterToMachineHealthCheck maps events from Cluster objects to
// MachineHealthCheck objects that belong to the Cluster
func (r *MachineHealthCheckReconciler) clusterToMachineHealthCheck(o handler.MapObject) []reconcile.Request {
	c, ok := o.Object.(*clusterv1.Cluster)
	if !ok {
		r.Log.Error(errors.New("incorrect type"), "expected a Cluster", "type", fmt.Sprintf("%T", o))
		return nil
	}

	mhcList := &clusterv1.MachineHealthCheckList{}
	if err := r.Client.List(
		context.TODO(),
		mhcList,
		client.InNamespace(c.Namespace),
		client.MatchingLabels{clusterv1.ClusterLabelName: c.Name},
	); err != nil {
		r.Log.Error(err, "Unable to list MachineHealthChecks", "cluster", c.Name, "namespace", c.Namespace)
		return nil
	}

	// This list should only contain MachineHealthChecks which belong to the given Cluster
	requests := []reconcile.Request{}
	for _, mhc := range mhcList.Items {
		key := types.NamespacedName{Namespace: mhc.Namespace, Name: mhc.Name}
		requests = append(requests, reconcile.Request{NamespacedName: key})
	}
	return requests
}

// machineToMachineHealthCheck maps events from Machine objects to
// MachineHealthCheck objects that monitor the given machine
func (r *MachineHealthCheckReconciler) machineToMachineHealthCheck(o handler.MapObject) []reconcile.Request {
	m, ok := o.Object.(*clusterv1.Machine)
	if !ok {
		r.Log.Error(errors.New("incorrect type"), "expected a Machine", "type", fmt.Sprintf("%T", o))
		return nil
	}

	mhcList := &clusterv1.MachineHealthCheckList{}
	if err := r.Client.List(
		context.Background(),
		mhcList,
		client.InNamespace(m.Namespace),
		client.MatchingLabels{clusterv1.ClusterLabelName: m.Spec.ClusterName},
	); err != nil {
		r.Log.Error(err, "Unable to list MachineHealthChecks", "machine", m.Name, "namespace", m.Namespace)
		return nil
	}

	var requests []reconcile.Request
	for k := range mhcList.Items {
		mhc := &mhcList.Items[k]
		if hasMatchingLabels(mhc.Spec.Selector, m.Labels) {
			key := util.ObjectKey(mhc)
			requests = append(requests, reconcile.Request{NamespacedName: key})
		}
	}
	return requests
}

func (r *MachineHealthCheckReconciler) nodeToMachineHealthCheck(o handler.MapObject) []reconcile.Request {
	node, ok := o.Object.(*corev1.Node)
	if !ok {
		r.Log.Error(errors.New("incorrect type"), "expected a Node", "type", fmt.Sprintf("%T", o))
		return nil
	}

	machine, err := r.getMachineFromNode(node.Name)
	if machine == nil || err != nil {
		r.Log.Error(err, "Unable to retrieve machine from node", "node", node.GetName())
		return nil
	}

	return r.machineToMachineHealthCheck(handler.MapObject{Object: machine})
}

func (r *MachineHealthCheckReconciler) getMachineFromNode(nodeName string) (*clusterv1.Machine, error) {
	machineList := &clusterv1.MachineList{}
	if err := r.Client.List(
		context.TODO(),
		machineList,
		client.MatchingFields{machineNodeNameIndex: nodeName},
	); err != nil {
		return nil, errors.Wrap(err, "failed getting machine list")
	}
	// TODO(vincepri): Remove this loop once controller runtime fake client supports
	// adding indexes on objects.
	items := []*clusterv1.Machine{}
	for i := range machineList.Items {
		machine := &machineList.Items[i]
		if machine.Status.NodeRef != nil && machine.Status.NodeRef.Name == nodeName {
			items = append(items, machine)
		}
	}
	if len(items) != 1 {
		return nil, errors.Errorf("expecting one machine for node %v, got %v", nodeName, machineNames(items))
	}
	return items[0], nil
}

func (r *MachineHealthCheckReconciler) watchClusterNodes(ctx context.Context, cluster *clusterv1.Cluster) error {
	// If there is no tracker, don't watch remote nodes
	if r.Tracker == nil {
		return nil
	}

	if err := r.Tracker.Watch(ctx, remote.WatchInput{
		Cluster:      util.ObjectKey(cluster),
		Watcher:      r.controller,
		Kind:         &corev1.Node{},
		EventHandler: &handler.EnqueueRequestsFromMapFunc{ToRequests: handler.ToRequestsFunc(r.nodeToMachineHealthCheck)},
	}); err != nil {
		return err
	}
	return nil
}

func (r *MachineHealthCheckReconciler) indexMachineByNodeName(object runtime.Object) []string {
	machine, ok := object.(*clusterv1.Machine)
	if !ok {
		r.Log.Error(errors.New("incorrect type"), "expected a Machine", "type", fmt.Sprintf("%T", object))
		return nil
	}

	if machine.Status.NodeRef != nil {
		return []string{machine.Status.NodeRef.Name}
	}

	return nil
}

// isAllowedRemediation checks the value of the MaxUnhealthy field to determine
// whether remediation should be allowed or not
func isAllowedRemediation(mhc *clusterv1.MachineHealthCheck) bool {
	if mhc.Spec.MaxUnhealthy == nil {
		return true
	}
	maxUnhealthy, err := intstr.GetValueFromIntOrPercent(mhc.Spec.MaxUnhealthy, int(mhc.Status.ExpectedMachines), false)
	if err != nil {
		return false
	}

	// If unhealthy is above maxUnhealthy, short circuit any further remediation
	unhealthy := mhc.Status.ExpectedMachines - mhc.Status.CurrentHealthy
	return int(unhealthy) <= maxUnhealthy
}

func machineNames(machines []*clusterv1.Machine) []string {
	result := make([]string, 0, len(machines))
	for _, m := range machines {
		result = append(result, m.Name)
	}
	return result
}
