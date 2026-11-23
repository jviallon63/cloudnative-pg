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

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/postgres"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/specs"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Sacrificial Pod detection", func() {
	car1 := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "car-1",
			Namespace: "default",
			Annotations: map[string]string{
				utils.ClusterSerialAnnotationName: "1",
			},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}

	car2 := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "car-2",
			Namespace: "default",
			Annotations: map[string]string{
				utils.ClusterSerialAnnotationName: "2",
			},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}

	foo := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "default",
			Annotations: map[string]string{
				utils.ClusterSerialAnnotationName: "3",
			},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionFalse,
				},
			},
		},
	}

	bar := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bar",
			Namespace: "default",
			Annotations: map[string]string{
				utils.ClusterSerialAnnotationName: "4",
			},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionFalse,
				},
			},
		},
	}

	// unreachable mimics a Pod whose node has stopped reporting to the API
	// server: the node lifecycle controller flips PodReady to False, but the
	// stale ContainersReady left by the kubelet remains True.
	unreachable := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unreachable",
			Namespace: "default",
			Annotations: map[string]string{
				utils.ClusterSerialAnnotationName: "5",
			},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.ContainersReady,
					Status: corev1.ConditionTrue,
				},
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionFalse,
				},
			},
		},
	}

	It("detects if the list of Pods is empty", func() {
		var podList []corev1.Pod
		Expect(findDeletableInstance(&apiv1.Cluster{}, podList)).To(BeEmpty())
	})

	It("detects if we have not a ready Pod", func() {
		podList := []corev1.Pod{foo, bar}
		Expect(findDeletableInstance(&apiv1.Cluster{}, podList)).To(BeEmpty())
	})

	It("detects it if is the first available", func() {
		podList := []corev1.Pod{foo, bar, car1, car2}
		resultName := findDeletableInstance(&apiv1.Cluster{}, podList)
		Expect(resultName).ToNot(BeEmpty())
		Expect(resultName).To(Equal("car-2"))
	})

	It("detects it if is not the first one", func() {
		podList := []corev1.Pod{car2, foo, bar, car1}
		resultName := findDeletableInstance(&apiv1.Cluster{}, podList)
		Expect(resultName).ToNot(BeEmpty())
		Expect(resultName).To(Equal("car-2"))
	})

	It("skips a Pod whose node is unreachable even if it has the highest serial", func() {
		podList := []corev1.Pod{car1, car2, unreachable}
		resultName := findDeletableInstance(&apiv1.Cluster{}, podList)
		Expect(resultName).To(Equal("car-2"))
	})
})

var _ = Describe("markOldPrimaryAsUnhealthy", func() {
	var env *testingEnvironment

	BeforeEach(func() {
		env = buildTestEnvironment()
	})

	makePod := func(name, namespace, role string) corev1.Pod {
		pod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{},
			},
		}
		if role != "" {
			utils.SetInstanceRole(&pod.ObjectMeta, role)
		}
		return pod
	}

	It("changes the primary label from the old primary pod", func() {
		ctx := context.Background()
		namespace := newFakeNamespace(env.client)

		primary := makePod("cluster-1", namespace, specs.ClusterRoleLabelPrimary)
		replica1 := makePod("cluster-2", namespace, specs.ClusterRoleLabelReplica)
		replica2 := makePod("cluster-3", namespace, specs.ClusterRoleLabelReplica)

		for i, pod := range []corev1.Pod{primary, replica1, replica2} {
			p := pod
			Expect(env.client.Create(ctx, &p)).To(Succeed())
			// refresh the local copy with server-assigned fields
			if i == 0 {
				primary = p
			}
		}

		pods := []corev1.Pod{primary, replica1, replica2}

		err := env.clusterReconciler.markOldPrimaryAsUnhealthy(ctx, "cluster-1", pods)
		Expect(err).ToNot(HaveOccurred())

		// Verify the old primary's label was changed to unhealthy on the API server
		var updated corev1.Pod
		Expect(env.client.Get(ctx, client.ObjectKeyFromObject(&primary), &updated)).To(Succeed())
		Expect(updated.Labels[utils.ClusterInstanceRoleLabelName]).To(Equal(specs.ClusterRoleLabelUnhealthy))
		//nolint:staticcheck
		Expect(updated.Labels[utils.ClusterRoleLabelName]).To(Equal(specs.ClusterRoleLabelUnhealthy))

		// Verify replica pods are unchanged
		var replica1Updated corev1.Pod
		Expect(env.client.Get(ctx, client.ObjectKeyFromObject(&replica1), &replica1Updated)).To(Succeed())
		Expect(replica1Updated.Labels[utils.ClusterInstanceRoleLabelName]).To(Equal(specs.ClusterRoleLabelReplica))
	})

	It("does not error when the old primary is not in the pod list", func() {
		ctx := context.Background()
		namespace := newFakeNamespace(env.client)

		replica := makePod("cluster-2", namespace, specs.ClusterRoleLabelReplica)
		Expect(env.client.Create(ctx, &replica)).To(Succeed())

		err := env.clusterReconciler.markOldPrimaryAsUnhealthy(ctx, "cluster-1", []corev1.Pod{replica})
		Expect(err).ToNot(HaveOccurred())
	})

	It("is a no-op when the old primary already has the unhealthy label", func() {
		ctx := context.Background()
		namespace := newFakeNamespace(env.client)

		pod := makePod("cluster-1", namespace, specs.ClusterRoleLabelUnhealthy)
		Expect(env.client.Create(ctx, &pod)).To(Succeed())

		err := env.clusterReconciler.markOldPrimaryAsUnhealthy(ctx, "cluster-1", []corev1.Pod{pod})
		Expect(err).ToNot(HaveOccurred())

		var updated corev1.Pod
		Expect(env.client.Get(ctx, client.ObjectKeyFromObject(&pod), &updated)).To(Succeed())
		Expect(updated.Labels[utils.ClusterInstanceRoleLabelName]).To(Equal(specs.ClusterRoleLabelUnhealthy))
	})

	It("surfaces the Patch error so callers can apply their best-effort or retry strategy", func() {
		ctx := context.Background()
		namespace := newFakeNamespace(env.client)

		primary := makePod("cluster-1", namespace, specs.ClusterRoleLabelPrimary)

		failingClient := fake.NewClientBuilder().
			WithScheme(env.scheme).
			WithObjects(&primary).
			WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(_ context.Context, _ client.WithWatch, obj client.Object,
					_ client.Patch, _ ...client.PatchOption,
				) error {
					Expect(obj).To(BeAssignableToTypeOf(&corev1.Pod{}))
					Expect(obj.GetName()).To(Equal("cluster-1"))
					Expect(obj.GetNamespace()).To(Equal(namespace))
					return fmt.Errorf("simulated API server error")
				},
			}).
			Build()

		r := &ClusterReconciler{Client: failingClient, Scheme: env.scheme}

		err := r.markOldPrimaryAsUnhealthy(ctx, "cluster-1", []corev1.Pod{primary})
		Expect(err).To(MatchError(ContainSubstring("simulated API server error")))
	})
})

var _ = Describe("Check pods not on primary node", func() {
	item1 := postgres.PostgresqlStatus{
		IsPrimary: false,
		Node:      "node-1",
		Pod:       &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-1"}},
	}

	item2 := postgres.PostgresqlStatus{
		IsPrimary: false,
		Node:      "node-2",
		Pod:       &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-2"}},
	}
	statusList := postgres.PostgresqlStatusList{Items: []postgres.PostgresqlStatus{item1, item2}}

	It("if primary is nil", func() {
		Expect(GetPodsNotOnPrimaryNode(statusList, nil).Items).To(BeEmpty())
	})

	item1.IsPrimary = true
	statusList2 := postgres.PostgresqlStatusList{Items: []postgres.PostgresqlStatus{item1, item2}}

	It("first status element is primary", func() {
		Expect(GetPodsNotOnPrimaryNode(statusList2, &statusList2.Items[0]).Items).ToNot(BeEmpty())
	})
})

var _ = Describe("requiredHealthyStandbysAfterSwitchover", func() {
	newCluster := func(instances int, sync *apiv1.SynchronousReplicaConfiguration, minSync int) *apiv1.Cluster {
		return &apiv1.Cluster{
			Spec: apiv1.ClusterSpec{
				Instances:       instances,
				MinSyncReplicas: minSync,
				PostgresConfiguration: apiv1.PostgresConfiguration{
					Synchronous: sync,
				},
			},
		}
	}

	It("requires nothing when synchronous replication is not configured", func() {
		Expect(requiredHealthyStandbysAfterSwitchover(newCluster(3, nil, 0))).To(Equal(0))
	})

	It("requires the configured number of synchronous standbys with dataDurability required", func() {
		cluster := newCluster(4, &apiv1.SynchronousReplicaConfiguration{
			Method:         apiv1.SynchronousReplicaConfigurationMethodAny,
			Number:         2,
			DataDurability: apiv1.DataDurabilityLevelRequired,
		}, 0)
		Expect(requiredHealthyStandbysAfterSwitchover(cluster)).To(Equal(2))
	})

	It("treats an empty dataDurability as required", func() {
		cluster := newCluster(3, &apiv1.SynchronousReplicaConfiguration{
			Method: apiv1.SynchronousReplicaConfigurationMethodAny,
			Number: 1,
		}, 0)
		Expect(requiredHealthyStandbysAfterSwitchover(cluster)).To(Equal(1))
	})

	It("requires nothing with dataDurability preferred", func() {
		cluster := newCluster(3, &apiv1.SynchronousReplicaConfiguration{
			Method:         apiv1.SynchronousReplicaConfigurationMethodAny,
			Number:         1,
			DataDurability: apiv1.DataDurabilityLevelPreferred,
		}, 0)
		Expect(requiredHealthyStandbysAfterSwitchover(cluster)).To(Equal(0))
	})

	It("requires nothing when external standby names are configured", func() {
		cluster := newCluster(3, &apiv1.SynchronousReplicaConfiguration{
			Method:          apiv1.SynchronousReplicaConfigurationMethodAny,
			Number:          1,
			StandbyNamesPre: []string{"external"},
		}, 0)
		Expect(requiredHealthyStandbysAfterSwitchover(cluster)).To(Equal(0))
	})

	It("uses minSyncReplicas with the legacy configuration", func() {
		Expect(requiredHealthyStandbysAfterSwitchover(newCluster(4, nil, 2))).To(Equal(2))
	})

	It("never requires more standbys than a switchover can leave available", func() {
		// with 3 instances, only one standby other than the new primary
		// and the demoted one can be available
		Expect(requiredHealthyStandbysAfterSwitchover(newCluster(3, nil, 2))).To(Equal(1))
		// with 2 instances, no standby other than the demoted primary exists
		cluster := newCluster(2, &apiv1.SynchronousReplicaConfiguration{
			Method: apiv1.SynchronousReplicaConfigurationMethodAny,
			Number: 1,
		}, 0)
		Expect(requiredHealthyStandbysAfterSwitchover(cluster)).To(Equal(0))
	})
})

var _ = Describe("countHealthyStandbysAfterSwitchover", func() {
	readyPod := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			},
		}
	}
	draining := func(node string) bool { return node == "draining" }

	It("counts only ready and streaming standbys on schedulable nodes, excluding the target", func() {
		notReady := readyPod("not-ready")
		notReady.Status.Conditions = nil
		standbys := postgres.PostgresqlStatusList{Items: []postgres.PostgresqlStatus{
			{Pod: readyPod("target"), Node: "ok", IsWalReceiverActive: true},
			{Pod: readyPod("healthy"), Node: "ok", IsWalReceiverActive: true},
			{Pod: readyPod("on-draining-node"), Node: "draining", IsWalReceiverActive: true},
			{Pod: readyPod("not-streaming"), Node: "ok", IsWalReceiverActive: false},
			{Pod: notReady, Node: "ok", IsWalReceiverActive: true},
		}}
		Expect(countHealthyStandbysAfterSwitchover(standbys, "target", draining)).To(Equal(1))
	})
})

var _ = Describe("setPrimaryOnSchedulableNode and synchronous replication", func() {
	var env *testingEnvironment

	BeforeEach(func() {
		env = buildTestEnvironment()
	})

	createNode := func(ctx context.Context, name string, unschedulable bool) {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       corev1.NodeSpec{Unschedulable: unschedulable},
		}
		Expect(env.client.Create(ctx, node)).To(Succeed())
	}

	instanceStatus := func(name, node string, isPrimary, ready bool) postgres.PostgresqlStatus {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if ready {
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		return postgres.PostgresqlStatus{
			Pod:                 pod,
			Node:                node,
			IsPrimary:           isPrimary,
			IsPodReady:          ready,
			IsWalReceiverActive: !isPrimary && ready,
		}
	}

	setup := func(ctx context.Context, mutators ...func(*apiv1.Cluster)) *apiv1.Cluster {
		namespace := newFakeNamespace(env.client)
		mutators = append([]func(*apiv1.Cluster){func(cluster *apiv1.Cluster) {
			cluster.Spec.PostgresConfiguration.Synchronous = &apiv1.SynchronousReplicaConfiguration{
				Method:         apiv1.SynchronousReplicaConfigurationMethodAny,
				Number:         1,
				DataDurability: apiv1.DataDurabilityLevelRequired,
			}
			cluster.Status.ReadyInstances = 3
			cluster.Status.CurrentPrimary = "pod-1"
			cluster.Status.TargetPrimary = "pod-1"
		}}, mutators...)
		return newFakeCNPGCluster(env.client, namespace, mutators...)
	}

	It("waits when the remaining standbys are all on draining nodes", func(ctx SpecContext) {
		// This is the parallel MachineDeployment rollout scenario: pod-3 has
		// been moved to a fresh node, but pod-2 is still on a node being drained
		suffix := rand.String(6)
		createNode(ctx, "node-1-"+suffix, true)
		createNode(ctx, "node-2-"+suffix, true)
		createNode(ctx, "node-3-"+suffix, false)
		cluster := setup(ctx)

		status := postgres.PostgresqlStatusList{Items: []postgres.PostgresqlStatus{
			instanceStatus("pod-1", "node-1-"+suffix, true, true),
			instanceStatus("pod-3", "node-3-"+suffix, false, true),
			instanceStatus("pod-2", "node-2-"+suffix, false, true),
		}}

		newPrimary, err := env.clusterReconciler.setPrimaryOnSchedulableNode(ctx, cluster, status, &status.Items[0])
		Expect(err).ToNot(HaveOccurred())
		Expect(newPrimary).To(BeEmpty())
	})

	It("waits when a standby is failed even if the bypass for failed pods applies", func(ctx SpecContext) {
		suffix := rand.String(6)
		createNode(ctx, "node-1-"+suffix, true)
		createNode(ctx, "node-2-"+suffix, false)
		cluster := setup(ctx, func(cluster *apiv1.Cluster) {
			cluster.Status.ReadyInstances = 2
			cluster.Status.InstancesStatus = map[apiv1.PodStatus][]string{
				apiv1.PodHealthy: {"pod-1", "pod-2"},
				apiv1.PodFailed:  {"pod-3"},
			}
		})

		status := postgres.PostgresqlStatusList{Items: []postgres.PostgresqlStatus{
			instanceStatus("pod-1", "node-1-"+suffix, true, true),
			instanceStatus("pod-2", "node-2-"+suffix, false, true),
		}}

		newPrimary, err := env.clusterReconciler.setPrimaryOnSchedulableNode(ctx, cluster, status, &status.Items[0])
		Expect(err).ToNot(HaveOccurred())
		Expect(newPrimary).To(BeEmpty())
	})

	It("switches over when enough standbys remain on schedulable nodes", func(ctx SpecContext) {
		suffix := rand.String(6)
		createNode(ctx, "node-1-"+suffix, true)
		createNode(ctx, "node-2-"+suffix, false)
		createNode(ctx, "node-3-"+suffix, false)
		cluster := setup(ctx)

		status := postgres.PostgresqlStatusList{Items: []postgres.PostgresqlStatus{
			instanceStatus("pod-1", "node-1-"+suffix, true, true),
			instanceStatus("pod-3", "node-3-"+suffix, false, true),
			instanceStatus("pod-2", "node-2-"+suffix, false, true),
		}}

		newPrimary, err := env.clusterReconciler.setPrimaryOnSchedulableNode(ctx, cluster, status, &status.Items[0])
		Expect(err).ToNot(HaveOccurred())
		Expect(newPrimary).To(Equal("pod-3"))
	})

	It("keeps the previous behavior with dataDurability preferred", func(ctx SpecContext) {
		suffix := rand.String(6)
		createNode(ctx, "node-1-"+suffix, true)
		createNode(ctx, "node-2-"+suffix, true)
		createNode(ctx, "node-3-"+suffix, false)
		cluster := setup(ctx, func(cluster *apiv1.Cluster) {
			cluster.Spec.PostgresConfiguration.Synchronous.DataDurability = apiv1.DataDurabilityLevelPreferred
		})

		status := postgres.PostgresqlStatusList{Items: []postgres.PostgresqlStatus{
			instanceStatus("pod-1", "node-1-"+suffix, true, true),
			instanceStatus("pod-3", "node-3-"+suffix, false, true),
			instanceStatus("pod-2", "node-2-"+suffix, false, true),
		}}

		newPrimary, err := env.clusterReconciler.setPrimaryOnSchedulableNode(ctx, cluster, status, &status.Items[0])
		Expect(err).ToNot(HaveOccurred())
		Expect(newPrimary).To(Equal("pod-3"))
	})
})
