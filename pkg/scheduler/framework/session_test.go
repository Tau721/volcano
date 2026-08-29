package framework

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"

	"volcano.sh/apis/pkg/apis/scheduling"
	topologyv1alpha1 "volcano.sh/apis/pkg/apis/topology/v1alpha1"
	"volcano.sh/volcano/pkg/scheduler/api"
)

func TestSession_adjustNetworkTopologySpec(t *testing.T) {
	tests := []struct {
		name         string
		jobs         map[api.JobID]*api.JobInfo
		nameMap      api.HyperNodeTierNameMap
		expectedJobs map[api.JobID]*api.JobInfo
	}{
		{
			name: "job with highestTierAllowed, no translation",
			jobs: map[api.JobID]*api.JobInfo{
				"test-uid": {
					PodGroup: &api.PodGroup{
						PodGroup: scheduling.PodGroup{
							Spec: scheduling.PodGroupSpec{
								NetworkTopology: &scheduling.NetworkTopologySpec{
									HighestTierName:    "",
									HighestTierAllowed: ptr.To(2),
								},
								SubGroupPolicy: []scheduling.SubGroupPolicySpec{
									{
										NetworkTopology: &scheduling.NetworkTopologySpec{
											HighestTierName:    "",
											HighestTierAllowed: ptr.To(1),
										},
									},
								},
							},
						},
					},
					SubJobs: map[api.SubJobID]*api.SubJobInfo{
						"test-uid": {
							NetworkTopology: &scheduling.NetworkTopologySpec{
								HighestTierName:    "",
								HighestTierAllowed: ptr.To(1),
							},
						},
					},
				},
			},
			nameMap: api.HyperNodeTierNameMap{
				"volcano.sh/hypernode":    1,
				"volcano.sh/hypercluster": 2,
			},
			expectedJobs: map[api.JobID]*api.JobInfo{
				"test-uid": {
					PodGroup: &api.PodGroup{
						PodGroup: scheduling.PodGroup{
							Spec: scheduling.PodGroupSpec{
								NetworkTopology: &scheduling.NetworkTopologySpec{
									HighestTierName:    "",
									HighestTierAllowed: ptr.To(2),
								},
								SubGroupPolicy: []scheduling.SubGroupPolicySpec{
									{
										NetworkTopology: &scheduling.NetworkTopologySpec{
											HighestTierName:    "",
											HighestTierAllowed: ptr.To(1),
										},
									},
								},
							},
						},
					},
					SubJobs: map[api.SubJobID]*api.SubJobInfo{
						"test-uid": {
							NetworkTopology: &scheduling.NetworkTopologySpec{
								HighestTierName:    "",
								HighestTierAllowed: ptr.To(1),
							},
						},
					},
				},
			},
		},
		{
			name: "job with highestTierName, need translation",
			jobs: map[api.JobID]*api.JobInfo{
				"test-uid": {
					PodGroup: &api.PodGroup{
						PodGroup: scheduling.PodGroup{
							Spec: scheduling.PodGroupSpec{
								NetworkTopology: &scheduling.NetworkTopologySpec{
									HighestTierName:    "volcano.sh/hypercluster",
									HighestTierAllowed: nil,
								},
								SubGroupPolicy: []scheduling.SubGroupPolicySpec{
									{
										NetworkTopology: &scheduling.NetworkTopologySpec{
											HighestTierName:    "volcano.sh/hypernode",
											HighestTierAllowed: nil,
										},
									},
								},
							},
						},
					},
					SubJobs: map[api.SubJobID]*api.SubJobInfo{
						"test-uid": {
							NetworkTopology: &scheduling.NetworkTopologySpec{
								HighestTierName:    "volcano.sh/hypernode",
								HighestTierAllowed: nil,
							},
						},
					},
				},
			},
			nameMap: api.HyperNodeTierNameMap{
				"volcano.sh/hypernode":    1,
				"volcano.sh/hypercluster": 2,
			},
			expectedJobs: map[api.JobID]*api.JobInfo{
				"test-uid": {
					PodGroup: &api.PodGroup{
						PodGroup: scheduling.PodGroup{
							Spec: scheduling.PodGroupSpec{
								NetworkTopology: &scheduling.NetworkTopologySpec{
									HighestTierName:    "",
									HighestTierAllowed: ptr.To(2),
								},
								SubGroupPolicy: []scheduling.SubGroupPolicySpec{
									{
										NetworkTopology: &scheduling.NetworkTopologySpec{
											HighestTierName:    "",
											HighestTierAllowed: ptr.To(1),
										},
									},
								},
							},
						},
					},
					SubJobs: map[api.SubJobID]*api.SubJobInfo{
						"test-uid": {
							NetworkTopology: &scheduling.NetworkTopologySpec{
								HighestTierName:    "",
								HighestTierAllowed: ptr.To(1),
							},
						},
					},
				},
			},
		},
		{
			name: "job with highestTierName, failed to translate",
			jobs: map[api.JobID]*api.JobInfo{
				"test-uid": {
					PodGroup: &api.PodGroup{
						PodGroup: scheduling.PodGroup{
							Spec: scheduling.PodGroupSpec{
								NetworkTopology: &scheduling.NetworkTopologySpec{
									HighestTierName:    "volcano.sh/hypercluster-test",
									HighestTierAllowed: nil,
								},
								SubGroupPolicy: []scheduling.SubGroupPolicySpec{
									{
										NetworkTopology: &scheduling.NetworkTopologySpec{
											HighestTierName:    "volcano.sh/hypernode-test",
											HighestTierAllowed: nil,
										},
									},
								},
							},
						},
					},
					SubJobs: map[api.SubJobID]*api.SubJobInfo{
						"test-uid": {
							NetworkTopology: &scheduling.NetworkTopologySpec{
								HighestTierName:    "volcano.sh/hypernode",
								HighestTierAllowed: ptr.To(1),
							},
						},
					},
				},
			},
			nameMap: api.HyperNodeTierNameMap{
				"volcano.sh/hypernode":    1,
				"volcano.sh/hypercluster": 2,
			},
			expectedJobs: map[api.JobID]*api.JobInfo{
				"test-uid": {
					PodGroup: &api.PodGroup{
						PodGroup: scheduling.PodGroup{
							Spec: scheduling.PodGroupSpec{
								NetworkTopology: &scheduling.NetworkTopologySpec{
									HighestTierName:    "volcano.sh/hypercluster-test",
									HighestTierAllowed: nil,
								},
								SubGroupPolicy: []scheduling.SubGroupPolicySpec{
									{
										NetworkTopology: &scheduling.NetworkTopologySpec{
											HighestTierName:    "volcano.sh/hypernode-test",
											HighestTierAllowed: nil,
										},
									},
								},
							},
						},
					},
					SubJobs: map[api.SubJobID]*api.SubJobInfo{
						"test-uid": {
							NetworkTopology: &scheduling.NetworkTopologySpec{
								HighestTierName:    "volcano.sh/hypernode",
								HighestTierAllowed: ptr.To(1),
							},
						},
					},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, job := range test.jobs {
				if job.PodGroup != nil && job.NetworkTopology == nil {
					job.NetworkTopology = job.PodGroup.Spec.NetworkTopology.DeepCopy()
				}
			}
			for _, job := range test.expectedJobs {
				if job.PodGroup != nil && job.NetworkTopology == nil {
					job.NetworkTopology = job.PodGroup.Spec.NetworkTopology.DeepCopy()
				}
			}
			ssn := &Session{
				Jobs:                 test.jobs,
				HyperNodeTierNameMap: test.nameMap,
			}
			ssn.adjustNetworkTopologySpec()
			for jobID, expectedJob := range test.expectedJobs {
				gotJob := ssn.Jobs[jobID]
				assert.Equal(t, expectedJob.NetworkTopology.HighestTierName,
					gotJob.NetworkTopology.HighestTierName, "job highestTierName should be equal")
				assert.Equal(t, expectedJob.NetworkTopology.HighestTierAllowed,
					gotJob.NetworkTopology.HighestTierAllowed, "job highestTierAllowed should be equal")
				for subJobID := range expectedJob.SubJobs {
					assert.Equal(t, expectedJob.SubJobs[subJobID].NetworkTopology.HighestTierName,
						gotJob.SubJobs[subJobID].NetworkTopology.HighestTierName, "subJob highestTierName should be equal")
					assert.Equal(t, expectedJob.SubJobs[subJobID].NetworkTopology.HighestTierAllowed,
						gotJob.SubJobs[subJobID].NetworkTopology.HighestTierAllowed, "subJob highestTierAllowed should be equal")
				}
			}
		})
	}
}

func TestAdjustNetworkTopologySpec_DoesNotMutatePodGroupSpec(t *testing.T) {
	maxTier := 4
	topHn := &topologyv1alpha1.HyperNode{}
	topHn.Name = ClusterTopHyperNode
	topHn.Spec.Tier = maxTier

	job := api.NewJobInfo("test-job")
	pg := &api.PodGroup{
		PodGroup: scheduling.PodGroup{
			Spec: scheduling.PodGroupSpec{
				MinMember: 4,
				NetworkTopology: &scheduling.NetworkTopologySpec{
					Mode:            scheduling.SoftNetworkTopologyMode,
					HighestTierName: "volcano.sh/hypercluster",
				},
				SubGroupPolicy: []scheduling.SubGroupPolicySpec{
					{
						Name:         "worker",
						SubGroupSize: ptr.To(int32(4)),
						NetworkTopology: &scheduling.NetworkTopologySpec{
							Mode:            scheduling.SoftNetworkTopologyMode,
							HighestTierName: "volcano.sh/hypernode",
						},
					},
				},
			},
		},
	}
	job.SetPodGroup(pg)
	job.SubJobs["test-job/worker/0"] = api.NewSubJobInfo("test-job/worker", "test-job/worker/0", job.UID, &pg.Spec.SubGroupPolicy[0], []string{"0"})

	originalJobTopology := job.PodGroup.Spec.NetworkTopology.DeepCopy()
	originalSubGroupTopology := job.PodGroup.Spec.SubGroupPolicy[0].NetworkTopology.DeepCopy()

	ssn := &Session{
		Jobs: map[api.JobID]*api.JobInfo{
			job.UID: job,
		},
		HyperNodeTierNameMap: api.HyperNodeTierNameMap{
			"volcano.sh/hypernode":    1,
			"volcano.sh/hypercluster": 2,
		},
		HyperNodes: api.HyperNodeInfoMap{
			ClusterTopHyperNode: api.NewHyperNodeInfo(topHn),
		},
	}

	ssn.adjustNetworkTopologySpec()

	assert.Equal(t, originalJobTopology, job.PodGroup.Spec.NetworkTopology)
	assert.Equal(t, originalSubGroupTopology, job.PodGroup.Spec.SubGroupPolicy[0].NetworkTopology)
	assert.Equal(t, scheduling.SoftNetworkTopologyMode, job.NetworkTopology.Mode)
	assert.Equal(t, ptr.To(2), job.NetworkTopology.HighestTierAllowed)
	assert.Equal(t, scheduling.SoftNetworkTopologyMode, job.SubJobs["test-job/worker/0"].NetworkTopology.Mode)
	assert.Equal(t, ptr.To(1), job.SubJobs["test-job/worker/0"].NetworkTopology.HighestTierAllowed)
}

func TestAdjustNetworkTopologySpec_NilPodGroup(t *testing.T) {
	job := api.NewJobInfo("test-job")
	ssn := &Session{Jobs: map[api.JobID]*api.JobInfo{job.UID: job}}
	// A nil PodGroup should not panic.
	ssn.adjustNetworkTopologySpec()
	assert.Nil(t, job.PodGroup, "PodGroup should remain nil")
}

func TestAdjustNetworkTopologySpec_PreservesTopologyMode(t *testing.T) {
	// Tier-name translation must not alter the declared hard/soft mode.
	maxTier := 4 // ClusterTopHyperNode tier will be max(existing tiers) + 1 = 3 + 1 = 4

	topHn := &topologyv1alpha1.HyperNode{}
	topHn.Name = ClusterTopHyperNode
	topHn.Spec.Tier = maxTier

	tests := []struct {
		name        string
		jobs        map[api.JobID]*api.JobInfo
		nameMap     api.HyperNodeTierNameMap
		hyperNodes  api.HyperNodeInfoMap
		wantJobMode scheduling.NetworkTopologyMode
		wantJobTier *int
	}{
		{
			name: "soft topology with tierName is translated and remains soft",
			jobs: map[api.JobID]*api.JobInfo{
				"test-uid": {
					PodGroup: &api.PodGroup{
						PodGroup: scheduling.PodGroup{
							Spec: scheduling.PodGroupSpec{
								NetworkTopology: &scheduling.NetworkTopologySpec{
									Mode:            scheduling.SoftNetworkTopologyMode,
									HighestTierName: "volcano.sh/hypercluster",
								},
							},
						},
					},
					SubJobs: map[api.SubJobID]*api.SubJobInfo{},
				},
			},
			nameMap: api.HyperNodeTierNameMap{
				"volcano.sh/hypernode":    1,
				"volcano.sh/hypercluster": 2,
			},
			hyperNodes: api.HyperNodeInfoMap{
				ClusterTopHyperNode: api.NewHyperNodeInfo(topHn),
			},
			wantJobMode: scheduling.SoftNetworkTopologyMode,
			wantJobTier: ptr.To(2),
		},
		{
			name: "pure soft topology without tierName remains unchanged",
			jobs: map[api.JobID]*api.JobInfo{
				"test-uid": {
					PodGroup: &api.PodGroup{
						PodGroup: scheduling.PodGroup{
							Spec: scheduling.PodGroupSpec{
								NetworkTopology: &scheduling.NetworkTopologySpec{
									Mode: scheduling.SoftNetworkTopologyMode,
								},
							},
						},
					},
					SubJobs: map[api.SubJobID]*api.SubJobInfo{},
				},
			},
			nameMap: api.HyperNodeTierNameMap{},
			hyperNodes: api.HyperNodeInfoMap{
				ClusterTopHyperNode: api.NewHyperNodeInfo(topHn),
			},
			wantJobMode: scheduling.SoftNetworkTopologyMode,
			wantJobTier: nil,
		},
		{
			name: "hard topology with tierName: only translated, not re-converted",
			jobs: map[api.JobID]*api.JobInfo{
				"test-uid": {
					PodGroup: &api.PodGroup{
						PodGroup: scheduling.PodGroup{
							Spec: scheduling.PodGroupSpec{
								NetworkTopology: &scheduling.NetworkTopologySpec{
									Mode:            scheduling.HardNetworkTopologyMode,
									HighestTierName: "volcano.sh/hypernode",
								},
							},
						},
					},
					SubJobs: map[api.SubJobID]*api.SubJobInfo{},
				},
			},
			nameMap: api.HyperNodeTierNameMap{
				"volcano.sh/hypernode":    1,
				"volcano.sh/hypercluster": 2,
			},
			hyperNodes: api.HyperNodeInfoMap{
				ClusterTopHyperNode: api.NewHyperNodeInfo(topHn),
			},
			wantJobMode: scheduling.HardNetworkTopologyMode,
			wantJobTier: ptr.To(1), // translated from tierName, not overwritten by maxTier
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, job := range tt.jobs {
				if job.PodGroup != nil && job.NetworkTopology == nil {
					job.NetworkTopology = job.PodGroup.Spec.NetworkTopology.DeepCopy()
				}
			}
			ssn := &Session{
				Jobs:                 tt.jobs,
				HyperNodeTierNameMap: tt.nameMap,
				HyperNodes:           tt.hyperNodes,
			}
			ssn.adjustNetworkTopologySpec()

			gotJob := ssn.Jobs["test-uid"]
			assert.Equal(t, tt.wantJobMode, gotJob.NetworkTopology.Mode, "job mode mismatch")
			assert.Equal(t, tt.wantJobTier, gotJob.NetworkTopology.HighestTierAllowed, "job tier mismatch")
		})
	}
}
