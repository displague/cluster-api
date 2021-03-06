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

	"github.com/pkg/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha3"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1alpha3"
	"sigs.k8s.io/cluster-api/controlplane/kubeadm/internal/hash"
	"sigs.k8s.io/cluster-api/controlplane/kubeadm/internal/machinefilters"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
)

// updateStatus is called after every reconcilitation loop in a defer statement to always make sure we have the
// resource status subresourcs up-to-date.
func (r *KubeadmControlPlaneReconciler) updateStatus(ctx context.Context, kcp *controlplanev1.KubeadmControlPlane, cluster *clusterv1.Cluster) error {
	selector := machinefilters.ControlPlaneSelectorForCluster(cluster.Name)
	// Copy label selector to its status counterpart in string format.
	// This is necessary for CRDs including scale subresources.
	kcp.Status.Selector = selector.String()

	ownedMachines, err := r.managementCluster.GetMachinesForCluster(ctx, util.ObjectKey(cluster), machinefilters.OwnedMachines(kcp))
	if err != nil {
		return errors.Wrap(err, "failed to get list of owned machines")
	}

	currentMachines := ownedMachines.Filter(machinefilters.MatchesConfigurationHash(hash.Compute(&kcp.Spec)))
	kcp.Status.UpdatedReplicas = int32(len(currentMachines))

	replicas := int32(len(ownedMachines))
	desiredReplicas := *kcp.Spec.Replicas

	// set basic data that does not require interacting with the workload cluster
	kcp.Status.Replicas = replicas
	kcp.Status.ReadyReplicas = 0
	kcp.Status.UnavailableReplicas = replicas

	switch {
	// We are scaling up
	case replicas < desiredReplicas:
		conditions.MarkFalse(kcp, controlplanev1.ResizedCondition, controlplanev1.ScalingUpReason, clusterv1.ConditionSeverityWarning, "Scaling up to %d replicas (actual %d)", desiredReplicas, replicas)
	// We are scaling down
	case replicas > desiredReplicas:
		conditions.MarkFalse(kcp, controlplanev1.ResizedCondition, controlplanev1.ScalingDownReason, clusterv1.ConditionSeverityWarning, "Scaling down to %d replicas (actual %d)", desiredReplicas, replicas)
	default:
		// make sure last resize operation is marked as completed.
		// NOTE: we are checking the number of machines ready so we report resize completed only when the machines
		// are actually provisioned (vs reporting completed immediately after the last machine object is created).
		readyMachines := ownedMachines.Filter(machinefilters.IsReady())
		if int32(len(readyMachines)) == replicas {
			conditions.MarkTrue(kcp, controlplanev1.ResizedCondition)
		}
	}

	// Return early if the deletion timestamp is set, we don't want to try to connect to the workload cluster.
	if !kcp.DeletionTimestamp.IsZero() {
		return nil
	}

	workloadCluster, err := r.managementCluster.GetWorkloadCluster(ctx, util.ObjectKey(cluster))
	if err != nil {
		return errors.Wrap(err, "failed to create remote cluster client")
	}
	status, err := workloadCluster.ClusterStatus(ctx)
	if err != nil {
		return err
	}
	kcp.Status.ReadyReplicas = status.ReadyNodes
	kcp.Status.UnavailableReplicas = replicas - status.ReadyNodes

	// This only gets initialized once and does not change if the kubeadm config map goes away.
	if status.HasKubeadmConfig {
		kcp.Status.Initialized = true
		conditions.MarkTrue(kcp, controlplanev1.AvailableCondition)
	}

	if kcp.Status.ReadyReplicas > 0 {
		kcp.Status.Ready = true
	}

	return nil
}
