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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("POD Disruption Budget specifications", func() {
	instancesNum := 3
	minAvailablePrimary := 1
	replicas := instancesNum - minAvailablePrimary
	minAvailableReplicas := replicas - 1
	cluster := &apiv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "thistest",
			Namespace: "default",
		},
		Spec: apiv1.ClusterSpec{
			ImageName: "postgres:18.0",
			Instances: instancesNum,
		},
	}

	It("have the same name as the PostgreSQL cluster", func() {
		result := BuildReplicasPodDisruptionBudget(cluster)
		Expect(result.Name).To(Equal(cluster.Name))
		Expect(result.Labels).To(BeEquivalentTo(map[string]string{
			utils.ClusterLabelName:                cluster.Name,
			utils.KubernetesAppLabelName:          utils.AppName,
			utils.KubernetesAppInstanceLabelName:  cluster.Name,
			utils.KubernetesAppVersionLabelName:   "18",
			utils.KubernetesAppComponentLabelName: utils.DatabaseComponentName,
			utils.KubernetesAppManagedByLabelName: utils.ManagerName,
		}))
		Expect(result.Namespace).To(Equal(cluster.Namespace))
	})

	It("require not more than one unavailable replicas", func() {
		result := BuildReplicasPodDisruptionBudget(cluster)
		Expect(result.Spec.MinAvailable.IntVal).To(Equal(int32(minAvailableReplicas)))
	})

	It("require at least one primary instance to be available at all times", func() {
		result := BuildPrimaryPodDisruptionBudget(cluster)
		Expect(result.Spec.MinAvailable.IntVal).To(Equal(int32(minAvailablePrimary)))
	})

	It("selects only replicas when no switchover or failover is in progress", func() {
		stable := cluster.DeepCopy()
		stable.Status.CurrentPrimary = "thistest-1"
		stable.Status.TargetPrimary = "thistest-1"

		result := BuildReplicasPodDisruptionBudget(stable)
		Expect(result.Spec.Selector.MatchLabels).To(HaveKeyWithValue(
			utils.ClusterInstanceRoleLabelName, ClusterRoleLabelReplica))
		Expect(result.Spec.Selector.MatchExpressions).To(BeEmpty())
		Expect(result.Spec.MinAvailable.IntVal).To(Equal(int32(minAvailableReplicas)))
	})

	It("blocks every non-primary disruption while a switchover or failover is in progress", func() {
		switchingOver := cluster.DeepCopy()
		switchingOver.Status.CurrentPrimary = "thistest-1"
		switchingOver.Status.TargetPrimary = "thistest-2"

		result := BuildReplicasPodDisruptionBudget(switchingOver)
		Expect(result.Spec.Selector.MatchLabels).To(Equal(map[string]string{
			utils.ClusterLabelName: cluster.Name,
		}))
		Expect(result.Spec.Selector.MatchExpressions).To(ConsistOf(metav1.LabelSelectorRequirement{
			Key:      utils.ClusterInstanceRoleLabelName,
			Operator: metav1.LabelSelectorOpIn,
			Values:   []string{ClusterRoleLabelReplica, ClusterRoleLabelUnhealthy},
		}))
		Expect(result.Spec.MinAvailable.String()).To(Equal("100%"))
	})

	It("does not consider the bootstrap of a new cluster as a switchover", func() {
		bootstrapping := cluster.DeepCopy()
		bootstrapping.Status.TargetPrimary = "thistest-1"

		result := BuildReplicasPodDisruptionBudget(bootstrapping)
		Expect(result.Spec.MinAvailable.IntVal).To(Equal(int32(minAvailableReplicas)))
	})
})
