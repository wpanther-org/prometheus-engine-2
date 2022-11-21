// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package operator

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	monitoringv1 "github.com/GoogleCloudPlatform/prometheus-engine/pkg/operator/apis/monitoring/v1"
	v1 "github.com/GoogleCloudPlatform/prometheus-engine/pkg/operator/apis/monitoring/v1"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	tclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type populateTargetsTestCase struct {
	name                  string
	prometheusTargets     []*prometheusv1.TargetsResult
	podMonitorings        []monitoringv1.PodMonitoring
	clusterPodMonitorings []monitoringv1.ClusterPodMonitoring
}

// Given a list of test cases on PodMonitoring, creates a new list containing
// those test cases and equivalent test cases for ClusterPodMonitoring and
// another equivalent set including both PodMonitoring and ClusterPodMonitoring.
func expandPopulateTargetsTestCases(testCases []populateTargetsTestCase) []populateTargetsTestCase {
	dataFinal := make([]populateTargetsTestCase, 0)
	for _, data := range testCases {
		if len(data.podMonitorings) == 0 {
			continue
		}
		clusterPrometheusTargets := make([]*prometheusv1.TargetsResult, 0, len(data.prometheusTargets))
		clusterPodMonitorings := make([]monitoringv1.ClusterPodMonitoring, 0, len(data.podMonitorings))
		for _, prometheusTarget := range data.prometheusTargets {
			if prometheusTarget == nil {
				clusterPrometheusTargets = append(clusterPrometheusTargets, nil)
				continue
			}
			clusterActive := make([]prometheusv1.ActiveTarget, 0, len(prometheusTarget.Active))
			for _, active := range prometheusTarget.Active {
				activeCluster := active
				activeCluster.ScrapePool = podMonitoringScrapePoolToClusterPodMonitoringScrapePool(active.ScrapePool)
				clusterActive = append(clusterActive, activeCluster)
			}
			prometheusTargetClusterPodMonitoring := &prometheusv1.TargetsResult{
				Active: clusterActive,
			}
			clusterPrometheusTargets = append(clusterPrometheusTargets, prometheusTargetClusterPodMonitoring)
		}
		for _, podMonitoring := range data.podMonitorings {
			copy := podMonitoring.DeepCopy()
			clusterPodMonitoring := monitoringv1.ClusterPodMonitoring{
				ObjectMeta: metav1.ObjectMeta{
					Name: copy.Name,
				},
				Spec: monitoringv1.ClusterPodMonitoringSpec{
					Selector:     copy.Spec.Selector,
					Endpoints:    copy.Spec.Endpoints,
					TargetLabels: copy.Spec.TargetLabels,
					Limits:       copy.Spec.Limits,
				},
				Status: copy.Status,
			}
			for endpointIndex, endpointStatus := range clusterPodMonitoring.Status.EndpointStatuses {
				clusterPodMonitoring.Status.EndpointStatuses[endpointIndex].Name = podMonitoringScrapePoolToClusterPodMonitoringScrapePool(endpointStatus.Name)
			}
			clusterPodMonitorings = append(clusterPodMonitorings, clusterPodMonitoring)
		}
		dataPodMonitorings := populateTargetsTestCase{
			name:              data.name + "-pod-monitoring",
			prometheusTargets: data.prometheusTargets,
			podMonitorings:    data.podMonitorings,
		}
		dataClusterPodMonitorings := populateTargetsTestCase{
			name:                  data.name + "-cluster-pod-monitoring",
			prometheusTargets:     clusterPrometheusTargets,
			clusterPodMonitorings: clusterPodMonitorings,
		}
		prometheusTargetsBoth := append(data.prometheusTargets, clusterPrometheusTargets...)
		dataBoth := populateTargetsTestCase{
			name:                  data.name + "-both",
			prometheusTargets:     prometheusTargetsBoth,
			podMonitorings:        data.podMonitorings,
			clusterPodMonitorings: data.clusterPodMonitorings,
		}
		dataFinal = append(dataFinal, dataPodMonitorings)
		dataFinal = append(dataFinal, dataClusterPodMonitorings)
		dataFinal = append(dataFinal, dataBoth)
	}
	return dataFinal
}

func podMonitoringScrapePoolToClusterPodMonitoringScrapePool(podMonitoringScrapePool string) string {
	scrapePool := podMonitoringScrapePool[len("PodMonitoring/"):]
	scrapePool = scrapePool[strings.Index(scrapePool, "/")+1:]
	return "ClusterPodMonitoring/" + scrapePool
}

func targetFetchFromMap(m map[string]*prometheusv1.TargetsResult) getTargetFn {
	return func(_ context.Context, _ logr.Logger, port int32, pod *corev1.Pod) (*prometheusv1.TargetsResult, error) {
		key := getPodKey(pod, port)
		targetsResult, ok := m[key]
		if !ok {
			return nil, fmt.Errorf("Pod target does not exist: %s", key)
		}
		return targetsResult, nil
	}
}

func TestPopulateTargets(t *testing.T) {
	scheme, err := getScheme()
	if err != nil {
		t.Fatal("Unable to get scheme")
	}

	var date = metav1.Date(2022, time.January, 4, 0, 0, 0, 0, time.UTC)

	testCases := expandPopulateTargetsTestCases([]populateTargetsTestCase{
		// All empty -- nothing happens.
		{
			name: "empty-monitorings",
		},
		// Single target, no monitorings -- nothing happens.
		{
			name: "single-target-no-monitorings",
			prometheusTargets: []*prometheusv1.TargetsResult{
				{
					Active: []prometheusv1.ActiveTarget{{
						Health:     "up",
						LastError:  "",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "a",
						}),
						LastScrapeDuration: 1.2,
					}},
				},
			},
		},
		// Single healthy target with no error, with matching PodMonitoring.
		{
			name: "single-healthy-target",
			prometheusTargets: []*prometheusv1.TargetsResult{
				{
					Active: []prometheusv1.ActiveTarget{{
						Health:     "up",
						LastError:  "",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "a",
						}),
						LastScrapeDuration: 1.2,
					}},
				},
			},
			podMonitorings: []monitoringv1.PodMonitoring{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "prom-example-1", Namespace: "gmp-test"},
					Spec: v1.PodMonitoringSpec{
						Endpoints: []v1.ScrapeEndpoint{{
							Port: intstr.FromString("metrics"),
						}},
					},
					Status: monitoringv1.PodMonitoringStatus{
						ObservedGeneration: 2,
						Conditions: []v1.MonitoringCondition{{
							Type:               monitoringv1.ConfigurationCreateSuccess,
							Status:             corev1.ConditionTrue,
							LastUpdateTime:     metav1.Time{},
							LastTransitionTime: metav1.Time{},
							Reason:             "",
							Message:            "",
						}},
						EndpointStatuses: []v1.ScrapeEndpointStatus{
							{
								Name:             "PodMonitoring/gmp-test/prom-example-1/metrics",
								ActiveTargets:    1,
								UnhealthyTargets: 0,
								LastUpdateTime:   date,
								SampleGroups: []v1.SampleGroup{
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health: "up",
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "a",
												},
												LastScrapeDurationSeconds: "1.2",
											},
										},
										Count: pointer.Int32(1),
									},
								},
								CollectorsFraction: "1",
							},
						},
					},
				}},
		},
		// Collectors target fetch failure.
		{
			name: "collectors-target-fetch-failure",
			prometheusTargets: []*prometheusv1.TargetsResult{
				nil,
				{
					Active: []prometheusv1.ActiveTarget{{
						Health:     "up",
						LastError:  "",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "a",
						}),
						LastScrapeDuration: 1.2,
					}},
				},
				{
					Active: []prometheusv1.ActiveTarget{{
						Health:     "up",
						LastError:  "",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-2/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "b",
						}),
						LastScrapeDuration: 2.4,
					}},
				},
				nil,
				nil,
			},
			podMonitorings: []monitoringv1.PodMonitoring{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "prom-example-1", Namespace: "gmp-test"},
					Spec: v1.PodMonitoringSpec{
						Endpoints: []v1.ScrapeEndpoint{{
							Port: intstr.FromString("metrics"),
						}},
					},
					Status: monitoringv1.PodMonitoringStatus{
						EndpointStatuses: []v1.ScrapeEndpointStatus{
							{
								Name:             "PodMonitoring/gmp-test/prom-example-1/metrics",
								ActiveTargets:    1,
								UnhealthyTargets: 0,
								LastUpdateTime:   date,
								SampleGroups: []v1.SampleGroup{
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health: "up",
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "a",
												},
												LastScrapeDurationSeconds: "1.2",
											},
										},
										Count: pointer.Int32(1),
									},
								},
								CollectorsFraction: "0.4",
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "prom-example-2", Namespace: "gmp-test"},
					Spec: v1.PodMonitoringSpec{
						Endpoints: []v1.ScrapeEndpoint{{
							Port: intstr.FromString("metrics"),
						}},
					},
					Status: monitoringv1.PodMonitoringStatus{
						EndpointStatuses: []v1.ScrapeEndpointStatus{
							{
								Name:             "PodMonitoring/gmp-test/prom-example-2/metrics",
								ActiveTargets:    1,
								UnhealthyTargets: 0,
								LastUpdateTime:   date,
								SampleGroups: []v1.SampleGroup{
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health: "up",
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "b",
												},
												LastScrapeDurationSeconds: "2.4",
											},
										},
										Count: pointer.Int32(1),
									},
								},
								CollectorsFraction: "0.4",
							},
						},
					},
				}},
		},
		// Single healthy target with no error, with non-matching PodMonitoring.
		{
			name: "single-healthy-target-no-match",
			prometheusTargets: []*prometheusv1.TargetsResult{
				{
					Active: []prometheusv1.ActiveTarget{{
						Health:     "up",
						LastError:  "",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-2/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "a",
						}),
						LastScrapeDuration: 1.2,
					}},
				},
			},
			podMonitorings: []monitoringv1.PodMonitoring{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "prom-example-1", Namespace: "gmp-test"},
					Spec: v1.PodMonitoringSpec{
						Endpoints: []v1.ScrapeEndpoint{{
							Port: intstr.FromString("metrics"),
						}},
					},
				}},
		},
		// Single healthy target with no error, with single matching PodMonitoring.
		{
			name: "single-healthy-target-matching",
			prometheusTargets: []*prometheusv1.TargetsResult{
				{
					Active: []prometheusv1.ActiveTarget{{
						Health:     "up",
						LastError:  "",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-2/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "a",
						}),
						LastScrapeDuration: 1.2,
					}},
				},
			},
			podMonitorings: []monitoringv1.PodMonitoring{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "prom-example-1", Namespace: "gmp-test"},
					Spec: v1.PodMonitoringSpec{
						Endpoints: []v1.ScrapeEndpoint{{
							Port: intstr.FromString("metrics"),
						}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "prom-example-2", Namespace: "gmp-test"},
					Spec: v1.PodMonitoringSpec{
						Endpoints: []v1.ScrapeEndpoint{{
							Port: intstr.FromString("metrics"),
						}},
					},
					Status: monitoringv1.PodMonitoringStatus{
						EndpointStatuses: []v1.ScrapeEndpointStatus{
							{
								Name:             "PodMonitoring/gmp-test/prom-example-2/metrics",
								ActiveTargets:    1,
								UnhealthyTargets: 0,
								LastUpdateTime:   date,
								SampleGroups: []v1.SampleGroup{
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health: "up",
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "a",
												},
												LastScrapeDurationSeconds: "1.2",
											},
										},
										Count: pointer.Int32(1),
									},
								},
								CollectorsFraction: "1",
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "prom-example-3", Namespace: "gmp-test"},
					Spec: v1.PodMonitoringSpec{
						Endpoints: []v1.ScrapeEndpoint{{
							Port: intstr.FromString("metrics"),
						}},
					},
				}},
		},
		// Single healthy target with an error, with matching PodMonitoring.
		{
			name: "single-healthy-target-with-error-matching",
			prometheusTargets: []*prometheusv1.TargetsResult{
				{
					Active: []prometheusv1.ActiveTarget{{
						Health:     "up",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "a",
						}),
						LastScrapeDuration: 1.2,
					}},
				},
			},
			podMonitorings: []monitoringv1.PodMonitoring{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "prom-example-1", Namespace: "gmp-test"},
					Spec: v1.PodMonitoringSpec{
						Endpoints: []v1.ScrapeEndpoint{{
							Port: intstr.FromString("metrics"),
						}},
					},
					Status: monitoringv1.PodMonitoringStatus{
						EndpointStatuses: []v1.ScrapeEndpointStatus{
							{
								Name:             "PodMonitoring/gmp-test/prom-example-1/metrics",
								ActiveTargets:    1,
								UnhealthyTargets: 0,
								LastUpdateTime:   date,
								SampleGroups: []v1.SampleGroup{
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health:    "up",
												LastError: pointer.String("err x"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "a",
												},
												LastScrapeDurationSeconds: "1.2",
											},
										},
										Count: pointer.Int32(1),
									},
								},
								CollectorsFraction: "1",
							},
						},
					},
				}},
		},
		// Single unhealthy target with an error, with matching PodMonitoring.
		{
			name: "single-unhealthy-target-with-error-matching",
			prometheusTargets: []*prometheusv1.TargetsResult{
				{
					Active: []prometheusv1.ActiveTarget{{
						Health:     "down",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "a",
						}),
						LastScrapeDuration: 1.2,
					}},
				},
			},
			podMonitorings: []monitoringv1.PodMonitoring{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "prom-example-1", Namespace: "gmp-test"},
					Spec: v1.PodMonitoringSpec{
						Endpoints: []v1.ScrapeEndpoint{{
							Port: intstr.FromString("metrics"),
						}},
					},
					Status: monitoringv1.PodMonitoringStatus{
						EndpointStatuses: []v1.ScrapeEndpointStatus{
							{
								Name:             "PodMonitoring/gmp-test/prom-example-1/metrics",
								ActiveTargets:    1,
								UnhealthyTargets: 1,
								LastUpdateTime:   date,
								SampleGroups: []v1.SampleGroup{
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health:    "down",
												LastError: pointer.String("err x"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "a",
												},
												LastScrapeDurationSeconds: "1.2",
											},
										},
										Count: pointer.Int32(1),
									},
								},
								CollectorsFraction: "1",
							},
						},
					},
				}},
		},
		// One healthy and one unhealthy target.
		{
			name: "single-healthy-single-unhealthy",
			prometheusTargets: []*prometheusv1.TargetsResult{
				{
					Active: []prometheusv1.ActiveTarget{{
						Health:     "down",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "b",
						}),
						LastScrapeDuration: 1.2,
					}, {
						Health:     "up",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "a",
						}),
						LastScrapeDuration: 4.3,
					}},
				},
			},
			podMonitorings: []monitoringv1.PodMonitoring{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "prom-example-1", Namespace: "gmp-test"},
					Spec: v1.PodMonitoringSpec{
						Endpoints: []v1.ScrapeEndpoint{{
							Port: intstr.FromString("metrics"),
						}},
					},
					Status: monitoringv1.PodMonitoringStatus{
						EndpointStatuses: []v1.ScrapeEndpointStatus{
							{
								Name:             "PodMonitoring/gmp-test/prom-example-1/metrics",
								ActiveTargets:    2,
								UnhealthyTargets: 1,
								LastUpdateTime:   date,
								SampleGroups: []v1.SampleGroup{
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health:    "down",
												LastError: pointer.String("err x"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "b",
												},
												LastScrapeDurationSeconds: "1.2",
											},
										},
										Count: pointer.Int32(1),
									},
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health: "up",
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "a",
												},
												LastScrapeDurationSeconds: "4.3",
											},
										},
										Count: pointer.Int32(1),
									},
								},
								CollectorsFraction: "1",
							},
						},
					},
				}},
		},
		// Multiple targets with multiple endpoints.
		{
			name: "multiple-targets-multiple-endpoints",
			prometheusTargets: []*prometheusv1.TargetsResult{
				{
					Active: []prometheusv1.ActiveTarget{{
						Health:     "down",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics-2",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "d",
						}),
						LastScrapeDuration: 3.6,
					}, {
						Health:     "down",
						LastError:  "err y",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics-1",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "b",
						}),
						LastScrapeDuration: 7.0,
					}, {
						Health:     "down",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics-1",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "a",
						}),
						LastScrapeDuration: 5.3,
					}, {
						Health:     "down",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics-2",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "c",
						}),
						LastScrapeDuration: 1.2,
					}},
				},
			},
			podMonitorings: []monitoringv1.PodMonitoring{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "prom-example-1", Namespace: "gmp-test"},
					Spec: v1.PodMonitoringSpec{
						Endpoints: []v1.ScrapeEndpoint{{
							Port: intstr.FromString("metrics-1"),
						}, {
							Port: intstr.FromString("metrics-2"),
						}},
					},
					Status: monitoringv1.PodMonitoringStatus{
						EndpointStatuses: []v1.ScrapeEndpointStatus{
							{
								Name:             "PodMonitoring/gmp-test/prom-example-1/metrics-1",
								ActiveTargets:    2,
								UnhealthyTargets: 2,
								LastUpdateTime:   date,
								SampleGroups: []v1.SampleGroup{
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health:    "down",
												LastError: pointer.String("err x"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "a",
												},
												LastScrapeDurationSeconds: "5.3",
											},
										},
										Count: pointer.Int32(1),
									},
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health:    "down",
												LastError: pointer.String("err y"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "b",
												},
												LastScrapeDurationSeconds: "7",
											},
										},
										Count: pointer.Int32(1),
									},
								},
								CollectorsFraction: "1",
							},
							{
								Name:             "PodMonitoring/gmp-test/prom-example-1/metrics-2",
								ActiveTargets:    2,
								UnhealthyTargets: 2,
								LastUpdateTime:   date,
								SampleGroups: []v1.SampleGroup{
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health:    "down",
												LastError: pointer.String("err x"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "c",
												},
												LastScrapeDurationSeconds: "1.2",
											},
											{
												Health:    "down",
												LastError: pointer.String("err x"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "d",
												},
												LastScrapeDurationSeconds: "3.6",
											},
										},
										Count: pointer.Int32(2),
									},
								},
								CollectorsFraction: "1",
							},
						},
					},
				}},
		},
		// Multiple unhealthy target with different errors.
		{
			name: "multiple-unhealthy-targets",
			prometheusTargets: []*prometheusv1.TargetsResult{
				{
					Active: []prometheusv1.ActiveTarget{{
						Health:     "down",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "f",
						}),
						LastScrapeDuration: 1.2,
					}, {
						Health:     "down",
						LastError:  "err y",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "c",
						}),
						LastScrapeDuration: 2.4,
					}, {
						Health:     "down",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "e",
						}),
						LastScrapeDuration: 3.6,
					}, {
						Health:     "down",
						LastError:  "err z",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "d",
						}),
						LastScrapeDuration: 4.7,
					}, {
						Health:     "down",
						LastError:  "err z",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "a",
						}),
						LastScrapeDuration: 5.0,
					}, {
						Health:     "down",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "b",
						}),
						LastScrapeDuration: 6.8,
					}},
				},
			},
			podMonitorings: []monitoringv1.PodMonitoring{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "prom-example-1", Namespace: "gmp-test"},
					Spec: v1.PodMonitoringSpec{
						Endpoints: []v1.ScrapeEndpoint{{
							Port: intstr.FromString("metrics"),
						}},
					},
					Status: monitoringv1.PodMonitoringStatus{
						EndpointStatuses: []v1.ScrapeEndpointStatus{
							{
								Name:             "PodMonitoring/gmp-test/prom-example-1/metrics",
								ActiveTargets:    6,
								UnhealthyTargets: 6,
								LastUpdateTime:   date,
								SampleGroups: []v1.SampleGroup{
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health:    "down",
												LastError: pointer.String("err x"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "b",
												},
												LastScrapeDurationSeconds: "6.8",
											},
											{
												Health:    "down",
												LastError: pointer.String("err x"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "e",
												},
												LastScrapeDurationSeconds: "3.6",
											},
											{
												Health:    "down",
												LastError: pointer.String("err x"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "f",
												},
												LastScrapeDurationSeconds: "1.2",
											},
										},
										Count: pointer.Int32(3),
									},
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health:    "down",
												LastError: pointer.String("err y"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "c",
												},
												LastScrapeDurationSeconds: "2.4",
											},
										},
										Count: pointer.Int32(1),
									},
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health:    "down",
												LastError: pointer.String("err z"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "a",
												},
												LastScrapeDurationSeconds: "5",
											},
											{
												Health:    "down",
												LastError: pointer.String("err z"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "d",
												},
												LastScrapeDurationSeconds: "4.7",
											},
										},
										Count: pointer.Int32(2),
									},
								},
								CollectorsFraction: "1",
							},
						},
					},
				}},
		},
		// Multiple unhealthy targets, one cut-off.
		{
			name: "multiple-unhealthy-targets-cut-off",
			prometheusTargets: []*prometheusv1.TargetsResult{
				{
					Active: []prometheusv1.ActiveTarget{{
						Health:     "down",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "f",
						}),
						LastScrapeDuration: 1.2,
					}, {
						Health:     "down",
						LastError:  "err y",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "c",
						}),
						LastScrapeDuration: 2.4,
					}, {
						Health:     "down",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "a",
						}),
						LastScrapeDuration: 3.6,
					}, {
						Health:     "down",
						LastError:  "err z",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "d",
						}),
						LastScrapeDuration: 4.7,
					}, {
						Health:     "down",
						LastError:  "err z",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "a",
						}),
						LastScrapeDuration: 5.0,
					}, {
						Health:     "down",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "b",
						}),
						LastScrapeDuration: 6.8,
					}, {
						Health:     "down",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "e",
						}),
						LastScrapeDuration: 4.1,
					}, {
						Health:     "down",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "f",
						}),
						LastScrapeDuration: 7.3,
					}, {
						Health:     "down",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "c",
						}),
						LastScrapeDuration: 2.7,
					}, {
						Health:     "down",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": "d",
						}),
						LastScrapeDuration: 9.5,
					}},
				},
			},
			podMonitorings: []monitoringv1.PodMonitoring{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "prom-example-1", Namespace: "gmp-test"},
					Spec: v1.PodMonitoringSpec{
						Endpoints: []v1.ScrapeEndpoint{{
							Port: intstr.FromString("metrics"),
						}},
					},
					Status: monitoringv1.PodMonitoringStatus{
						EndpointStatuses: []v1.ScrapeEndpointStatus{
							{
								Name:             "PodMonitoring/gmp-test/prom-example-1/metrics",
								ActiveTargets:    10,
								UnhealthyTargets: 10,
								LastUpdateTime:   date,
								SampleGroups: []v1.SampleGroup{
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health:    "down",
												LastError: pointer.String("err x"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "a",
												},
												LastScrapeDurationSeconds: "3.6",
											},
											{
												Health:    "down",
												LastError: pointer.String("err x"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "b",
												},
												LastScrapeDurationSeconds: "6.8",
											},
											{
												Health:    "down",
												LastError: pointer.String("err x"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "c",
												},
												LastScrapeDurationSeconds: "2.7",
											},
											{
												Health:    "down",
												LastError: pointer.String("err x"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "d",
												},
												LastScrapeDurationSeconds: "9.5",
											},
											{
												Health:    "down",
												LastError: pointer.String("err x"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "e",
												},
												LastScrapeDurationSeconds: "4.1",
											},
										},
										Count: pointer.Int32(7),
									},
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health:    "down",
												LastError: pointer.String("err y"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "c",
												},
												LastScrapeDurationSeconds: "2.4",
											},
										},
										Count: pointer.Int32(1),
									},
									{
										SampleTargets: []v1.SampleTarget{
											{
												Health:    "down",
												LastError: pointer.String("err z"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "a",
												},
												LastScrapeDurationSeconds: "5",
											},
											{
												Health:    "down",
												LastError: pointer.String("err z"),
												Labels: map[model.LabelName]model.LabelValue{
													"instance": "d",
												},
												LastScrapeDurationSeconds: "4.7",
											},
										},
										Count: pointer.Int32(2),
									},
								},
								CollectorsFraction: "1",
							},
						},
					},
				}},
		},
	})

	for _, testCase := range testCases {
		t.Run(fmt.Sprintf("target-status-conversion-%s", testCase.name), func(t *testing.T) {
			clientBuilder := fake.NewClientBuilder().WithScheme(scheme)
			for _, podMonitoring := range testCase.podMonitorings {
				copy := podMonitoring.DeepCopy()
				copy.GetStatus().EndpointStatuses = nil
				clientBuilder.WithObjects(copy)
			}
			for _, clusterPodMonitoring := range testCase.clusterPodMonitorings {
				copy := clusterPodMonitoring.DeepCopy()
				copy.GetStatus().EndpointStatuses = nil
				clientBuilder.WithObjects(copy)
			}

			kubeClient := clientBuilder.Build()

			err := populateTargets(context.Background(), testr.New(t), kubeClient, testCase.prometheusTargets)
			if err != nil {
				t.Fatalf("Failed to populate targets: %s", err)
			}

			for _, podMonitoring := range testCase.podMonitorings {
				var after monitoringv1.PodMonitoring
				if err := kubeClient.Get(context.Background(), types.NamespacedName{
					Namespace: podMonitoring.GetNamespace(),
					Name:      podMonitoring.GetName(),
				}, &after); err != nil {
					t.Fatal("Unable to find PodMonitoring:", podMonitoring.GetKey(), err)
				}
				normalizeEndpointStatuses(after.Status.EndpointStatuses, date)
				if !cmp.Equal(podMonitoring.Status, after.Status) {
					t.Errorf("PodMonitoring does not match: %s\n%s", podMonitoring.GetKey(), cmp.Diff(podMonitoring.Status, after.Status))
				}
			}

			for _, clusterPodMonitoring := range testCase.clusterPodMonitorings {
				var after monitoringv1.ClusterPodMonitoring
				if err := kubeClient.Get(context.Background(), types.NamespacedName{
					Name: clusterPodMonitoring.GetName(),
				}, &after); err != nil {
					t.Fatal("Unable to find ClusterPodMonitoring:", clusterPodMonitoring.GetKey(), err)
				}
				normalizeEndpointStatuses(after.Status.EndpointStatuses, date)
				if !cmp.Equal(clusterPodMonitoring.Status, after.Status) {
					t.Errorf("ClusterPodMonitoring does not match: %s\n%s", clusterPodMonitoring.GetKey(), cmp.Diff(clusterPodMonitoring.Status, after.Status))
				}
			}
		})
	}
}

func getPodKey(pod *corev1.Pod, port int32) string {
	return fmt.Sprintf("%s:%d", pod.Status.PodIP, port)
}

func normalizeEndpointStatuses(endpointStatuses []monitoringv1.ScrapeEndpointStatus, time metav1.Time) {
	for i := range endpointStatuses {
		endpointStatuses[i].LastUpdateTime = time
	}
}

// Test that polling propagates all the way through and only on ticks.
func TestPolling(t *testing.T) {
	ctx := context.Background()
	logger := testr.New(t)
	opts := Options{
		ProjectID:             "test-proj",
		Location:              "test-loc",
		Cluster:               "test-cluster",
		OperatorNamespace:     "gmp-system",
		TargetPollConcurrency: 4,
	}
	if err := opts.defaultAndValidate(logger); err != nil {
		t.Fatal("Invalid options:", err)
	}

	fakeClock := tclock.NewFakeClock(time.Now())

	scheme, err := getScheme()
	if err != nil {
		t.Fatal("Unable to get scheme")
	}

	port := int32(19090)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-a",
			Namespace: opts.OperatorNamespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "prometheus",
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "127.0.0.1",
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "prometheus",
				Ready: true,
			}},
		},
	}

	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameCollector,
			Namespace: opts.OperatorNamespace,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "prometheus",
						Ports: []corev1.ContainerPort{{
							Name:          "prom-metrics",
							ContainerPort: port,
						}},
					}},
				},
			},
		},
	}).WithObjects(&monitoringv1.PodMonitoring{
		ObjectMeta: metav1.ObjectMeta{Name: "prom-example-1", Namespace: "gmp-test"},
		Spec: v1.PodMonitoringSpec{
			Endpoints: []v1.ScrapeEndpoint{{
				Port: intstr.FromString("metrics"),
			}},
		},
	}).WithObjects(pod).Build()

	prometheusTargetMap := make(map[string]*prometheusv1.TargetsResult, 1)
	key := getPodKey(pod, port)
	prometheusTargetMap[key] = &prometheusv1.TargetsResult{
		Active: []prometheusv1.ActiveTarget{{
			Health: "up",
			Labels: map[model.LabelName]model.LabelValue{
				"instance": model.LabelValue("a"),
			},
			ScrapePool:         "PodMonitoring/gmp-test/prom-example-1/metrics",
			LastError:          "err x",
			LastScrapeDuration: 1.2,
		}},
	}

	ch := make(chan event.GenericEvent, 1)
	reconciler := &targetStatusReconciler{
		ch:         ch,
		opts:       opts,
		getTarget:  targetFetchFromMap(prometheusTargetMap),
		logger:     logger,
		kubeClient: kubeClient,
		clock:      fakeClock,
	}

	expectStatus := func(t *testing.T, description string, expected []monitoringv1.ScrapeEndpointStatus) {
		// Must poll because status is updated via other thread.
		var err error
		if pollErr := wait.Poll(100*time.Millisecond, 2*time.Second, func() (done bool, err error) {
			var podMonitorings monitoringv1.PodMonitoringList
			kubeClient.List(ctx, &podMonitorings)
			switch amount := len(podMonitorings.Items); amount {
			case 0:
				err = fmt.Errorf("Could not find %s PodMonitoring", description)
				return false, nil
			case 1:
				status := podMonitorings.Items[0].Status.EndpointStatuses
				normalizeEndpointStatuses(status, metav1.Time{})
				diff := cmp.Diff(status, expected)
				if diff != "" {
					err = fmt.Errorf("Expected %s endpoint statuses to be: %s", description, diff)
					return false, nil
				}
				return true, nil
			default:
				err = errors.Errorf("invalid PodMonitorings found: %d", amount)
				return false, err
			}
		}); pollErr != nil {
			t.Fatalf("Failed waiting for %s status: %s", description, err)
		}
	}

	// Status should be empty initially, until the reconciler starts.
	expectStatus(t, "initial", nil)

	go func() {
		// Emulate Kubernetes controller manager event handler behavior.
		ch <- event.GenericEvent{
			Object: &appsv1.DaemonSet{},
		}
		for range ch {
			reconciler.Reconcile(ctx, reconcile.Request{})
		}
	}()

	// First tick.
	fakeClock.Step(pollDurationMin)
	statusTick1 := []v1.ScrapeEndpointStatus{
		{
			Name:             "PodMonitoring/gmp-test/prom-example-1/metrics",
			ActiveTargets:    1,
			UnhealthyTargets: 0,
			SampleGroups: []v1.SampleGroup{
				{
					SampleTargets: []v1.SampleTarget{
						{
							Health: "up",
							Labels: map[model.LabelName]model.LabelValue{
								"instance": "a",
							},
							LastError:                 pointer.String("err x"),
							LastScrapeDurationSeconds: "1.2",
						},
					},
					Count: pointer.Int32(1),
				},
			},
			CollectorsFraction: "1",
		},
	}
	expectStatus(t, "first tick", statusTick1)

	active := &prometheusTargetMap[key].Active[0]
	active.Health = "down"
	active.LastError = "err y"
	active.LastScrapeDuration = 5.4
	// We didn't tick yet so we don't expect a change yet.
	expectStatus(t, "first wait", statusTick1)

	// Second tick.
	fakeClock.Step(pollDurationMin)
	statusTick2 := []v1.ScrapeEndpointStatus{
		{
			Name:             "PodMonitoring/gmp-test/prom-example-1/metrics",
			ActiveTargets:    1,
			UnhealthyTargets: 1,
			SampleGroups: []v1.SampleGroup{
				{
					SampleTargets: []v1.SampleTarget{
						{
							Health: "down",
							Labels: map[model.LabelName]model.LabelValue{
								"instance": "a",
							},
							LastError:                 pointer.String("err y"),
							LastScrapeDurationSeconds: "5.4",
						},
					},
					Count: pointer.Int32(1),
				},
			},
			CollectorsFraction: "1",
		},
	}
	expectStatus(t, "second tick", statusTick2)

	active = &prometheusTargetMap[key].Active[0]
	active.Health = "up"
	active.LastError = "err z"
	active.LastScrapeDuration = 8.3
	// We didn't tick yet so we don't expect a change yet.
	expectStatus(t, "second wait", statusTick2)

	fakeClock.Step(pollDurationMin)
	statusTick3 := []v1.ScrapeEndpointStatus{
		{
			Name:             "PodMonitoring/gmp-test/prom-example-1/metrics",
			ActiveTargets:    1,
			UnhealthyTargets: 0,
			SampleGroups: []v1.SampleGroup{
				{
					SampleTargets: []v1.SampleTarget{
						{
							Health: "up",
							Labels: map[model.LabelName]model.LabelValue{
								"instance": "a",
							},
							LastError:                 pointer.String("err z"),
							LastScrapeDurationSeconds: "8.3",
						},
					},
					Count: pointer.Int32(1),
				},
			},
			CollectorsFraction: "1",
		},
	}
	expectStatus(t, "third tick", statusTick3)
}

// Tests that for pod, targets are fetched correctly (concurrently).
func TestFetchTargets(t *testing.T) {
	ctx := context.Background()
	logger := testr.New(t)
	concurrency := uint16(4)
	opts := Options{
		ProjectID:             "test-proj",
		Location:              "test-loc",
		Cluster:               "test-cluster",
		TargetPollConcurrency: concurrency,
	}
	if err := opts.defaultAndValidate(logger); err != nil {
		t.Fatal("Invalid options:", err)
	}

	scheme, err := getScheme()
	if err != nil {
		t.Fatal("Unable to get scheme")
	}

	concurrencyInt := int(concurrency)
	// Test 0 where we have no pods to ensure the thread pool does not stall or
	// panic. Also sanity test that the thread pool can ingest at and over max
	// capacity.
	podCounts := []int{0, 1, 2, concurrencyInt - 1, concurrencyInt, concurrencyInt + 1, concurrencyInt * 3}
	for _, podCnt := range podCounts {
		t.Run(fmt.Sprintf("fetch-%d-pods", podCnt), func(t *testing.T) {
			port := int32(19090)
			prometheusTargetMap := make(map[string]*prometheusv1.TargetsResult, podCnt)
			targetsExpected := make([]*prometheusv1.TargetsResult, 0, podCnt)
			kubeClientBuilder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      NameCollector,
					Namespace: opts.OperatorNamespace,
				},
				Spec: appsv1.DaemonSetSpec{
					Selector: &metav1.LabelSelector{},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name: "prometheus",
								Ports: []corev1.ContainerPort{{
									Name:          "prom-metrics",
									ContainerPort: port,
								}},
							}},
						},
					},
				},
			})
			for i := 0; i < podCnt; i++ {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("pod-%d", i),
						Namespace: opts.OperatorNamespace,
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name: "prometheus",
						}},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						PodIP: fmt.Sprint(i),
						ContainerStatuses: []corev1.ContainerStatus{{
							Name:  "prometheus",
							Ready: true,
						}},
					},
				}
				kubeClientBuilder.WithObjects(pod)

				target := &prometheusv1.TargetsResult{
					Active: []prometheusv1.ActiveTarget{{
						Health:     "up",
						LastError:  "err x",
						ScrapePool: "PodMonitoring/gmp-test/prom-example-1/metrics",
						Labels: model.LabelSet(map[model.LabelName]model.LabelValue{
							"instance": model.LabelValue(fmt.Sprint(i)),
						}),
						LastScrapeDuration: 1.2,
					}},
				}
				prometheusTargetMap[getPodKey(pod, port)] = target

				targetsExpected = append(targetsExpected, target)
			}

			kubeClient := kubeClientBuilder.Build()

			targets, err := fetchTargets(ctx, logger, opts, targetFetchFromMap(prometheusTargetMap), kubeClient)
			if err != nil {
				t.Fatal("Unable to fetch targets", err)
			}

			// Concurrency causes the targets slice to come back randomly.
			sort.Slice(targets, func(i, j int) bool {
				lhsName := targets[i].Active[0].Labels["instance"]
				rhsName := targets[j].Active[0].Labels["instance"]
				lhsValue, err := strconv.Atoi(string(lhsName))
				if err != nil {
					return false
				}
				rhsValue, err := strconv.Atoi(string(rhsName))
				if err != nil {
					return false
				}
				return lhsValue < rhsValue
			})

			diff := cmp.Diff(targets, targetsExpected)
			if diff != "" {
				t.Errorf("Targets:")
				for i, target := range targets {
					t.Errorf("%d: %v", i, target)
				}
				t.Errorf("Targets Expected:")
				for i, target := range targetsExpected {
					t.Errorf("%d: %v", i, target)
				}
				t.Fatalf("Targets do not match expected: %s", diff)
			}
		})
	}
}
