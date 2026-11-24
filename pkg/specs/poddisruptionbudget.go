/*
Copyright © contributors to CloudNativePG, established as
CloudNativePG a Series of LF Projects, LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package specs

import (
	"fmt"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"
)

// BuildReplicasPodDisruptionBudget creates a pod disruption budget telling
// K8s to avoid removing more than one replica at a time.
// While a switchover or failover is in progress, instance role labels are
// in transition (the promoted instance may still be labelled as a replica
// and the demoted primary is labelled as unhealthy): in that window the
// PDB covers every non-primary instance and allows no voluntary disruption.
func BuildReplicasPodDisruptionBudget(cluster *apiv1.Cluster) *policyv1.PodDisruptionBudget {
	// We should ensure that in a cluster of n instances,
	// with n-1 replicas, at least n-2 are always available
	if cluster == nil || cluster.Spec.Instances < 3 {
		return nil
	}
	minAvailableReplicas := int32(cluster.Spec.Instances - 2) //nolint:gosec
	allReplicasButOne := intstr.FromInt32(minAvailableReplicas)
	version, _ := cluster.GetPostgresqlMajorVersion()

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				utils.KubernetesAppLabelName:          utils.AppName,
				utils.KubernetesAppInstanceLabelName:  cluster.Name,
				utils.KubernetesAppVersionLabelName:   fmt.Sprint(version),
				utils.KubernetesAppComponentLabelName: utils.DatabaseComponentName,
				utils.KubernetesAppManagedByLabelName: utils.ManagerName,
			},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					utils.ClusterLabelName:             cluster.Name,
					utils.ClusterInstanceRoleLabelName: ClusterRoleLabelReplica,
				},
			},
			MinAvailable: &allReplicasButOne,
		},
	}

	if isPrimaryTransitionInProgress(cluster) {
		allInstances := intstr.FromString("100%")
		pdb.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				utils.ClusterLabelName: cluster.Name,
			},
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      utils.ClusterInstanceRoleLabelName,
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{ClusterRoleLabelReplica, ClusterRoleLabelUnhealthy},
				},
			},
		}
		pdb.Spec.MinAvailable = &allInstances
	}

	cluster.SetInheritedDataAndOwnership(&pdb.ObjectMeta)

	return pdb
}

// BuildPrimaryPodDisruptionBudget creates a pod disruption budget, telling
// K8s to avoid removing more than one primary instance at a time
func BuildPrimaryPodDisruptionBudget(cluster *apiv1.Cluster) *policyv1.PodDisruptionBudget {
	if cluster == nil {
		return nil
	}
	one := intstr.FromInt32(1)

	version, _ := cluster.GetPostgresqlMajorVersion()

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name + apiv1.PrimaryPodDisruptionBudgetSuffix,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				utils.KubernetesAppLabelName:          utils.AppName,
				utils.KubernetesAppInstanceLabelName:  cluster.Name,
				utils.KubernetesAppVersionLabelName:   fmt.Sprint(version),
				utils.KubernetesAppComponentLabelName: utils.DatabaseComponentName,
				utils.KubernetesAppManagedByLabelName: utils.ManagerName,
			},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					utils.ClusterLabelName:             cluster.Name,
					utils.ClusterInstanceRoleLabelName: ClusterRoleLabelPrimary,
				},
			},
			MinAvailable: &one,
		},
	}

	cluster.SetInheritedDataAndOwnership(&pdb.ObjectMeta)

	return pdb
}

// isPrimaryTransitionInProgress returns true when a switchover or a failover
// has been requested and not yet completed
func isPrimaryTransitionInProgress(cluster *apiv1.Cluster) bool {
	return cluster.Status.CurrentPrimary != "" &&
		cluster.Status.TargetPrimary != cluster.Status.CurrentPrimary
}
