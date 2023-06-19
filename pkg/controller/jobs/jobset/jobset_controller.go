/*
Copyright 2022 The Kubernetes Authors.

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

package jobset

import (
	"context"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	jobsetapi "sigs.k8s.io/jobset/api/jobset/v1alpha2"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	"sigs.k8s.io/kueue/pkg/controller/jobframework"
	"sigs.k8s.io/kueue/pkg/util/maps"
)

var (
	gvk           = jobsetapi.GroupVersion.WithKind("JobSet")
	FrameworkName = "jobset.x-k8s.io/jobset"
)

func init() {
	utilruntime.Must(jobframework.RegisterIntegration(FrameworkName, jobframework.IntegrationCallbacks{
		SetupIndexes:           SetupIndexes,
		NewReconciler:          NewReconciler,
		SetupWebhook:           SetupJobSetWebhook,
		JobType:                &jobsetapi.JobSet{},
		AddToScheme:            jobsetapi.AddToScheme,
		IsManagingObjectsOwner: isJobSet,
	}))
}

//+kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=list;get;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;watch;update;patch
//+kubebuilder:rbac:groups=jobset.x-k8s.io,resources=jobsets,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=jobset.x-k8s.io,resources=jobsets/status,verbs=get;update
//+kubebuilder:rbac:groups=kueue.x-k8s.io,resources=workloads,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kueue.x-k8s.io,resources=workloads/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kueue.x-k8s.io,resources=workloads/finalizers,verbs=update
//+kubebuilder:rbac:groups=kueue.x-k8s.io,resources=resourceflavors,verbs=get;list;watch

var NewReconciler = jobframework.NewGenericReconciler(func() jobframework.GenericJob { return &JobSet{} }, nil)

func isJobSet(owner *metav1.OwnerReference) bool {
	return owner.Kind == "JobSet" && strings.HasPrefix(owner.APIVersion, "jobset.x-k8s.io/v1alpha2")
}

type JobSet jobsetapi.JobSet

var _ jobframework.GenericJob = (*JobSet)(nil)

func fromObject(obj runtime.Object) *JobSet {
	return (*JobSet)(obj.(*jobsetapi.JobSet))
}

func (j *JobSet) Object() client.Object {
	return (*jobsetapi.JobSet)(j)
}

func (j *JobSet) IsSuspended() bool {
	return pointer.BoolDeref(j.Spec.Suspend, false)
}

func (j *JobSet) IsActive() bool {
	// ToDo implement from jobset side jobset.status.Active != 0
	return !j.IsSuspended()
}

func (j *JobSet) Suspend() {
	j.Spec.Suspend = pointer.Bool(true)
}

func (j *JobSet) ResetStatus() bool {
	return false
}

func (j *JobSet) GetGVK() schema.GroupVersionKind {
	return gvk
}

func (j *JobSet) PodSets() []kueue.PodSet {
	podSets := make([]kueue.PodSet, len(j.Spec.ReplicatedJobs))
	for index, replicatedJob := range j.Spec.ReplicatedJobs {
		podSets[index] = kueue.PodSet{
			Name:     replicatedJob.Name,
			Template: *replicatedJob.Template.Spec.Template.DeepCopy(),
			Count:    podsCount(&replicatedJob),
		}
	}
	return podSets
}

func (j *JobSet) RunWithPodSetsInfo(podSetInfos []jobframework.PodSetInfo) {
	j.Spec.Suspend = pointer.Bool(false)
	if len(podSetInfos) == 0 {
		return
	}

	// for initially unsuspend, this should be enough however if the jobs are already created
	// the node selectors should be updated on each of them
	for index := range j.Spec.ReplicatedJobs {
		templateSpec := &j.Spec.ReplicatedJobs[index].Template.Spec.Template.Spec
		templateSpec.NodeSelector = maps.MergeKeepFirst(podSetInfos[index].NodeSelector, templateSpec.NodeSelector)
	}
}

func (j *JobSet) RestorePodSetsInfo(podSetInfos []jobframework.PodSetInfo) {
	if len(podSetInfos) == 0 {
		return
	}
	for index := range j.Spec.ReplicatedJobs {
		if equality.Semantic.DeepEqual(j.Spec.ReplicatedJobs[index].Template.Spec.Template.Spec.NodeSelector, podSetInfos[index].NodeSelector) {
			continue
		}
		j.Spec.ReplicatedJobs[index].Template.Spec.Template.Spec.NodeSelector = maps.Clone(podSetInfos[index].NodeSelector)
	}
}

func (j *JobSet) Finished() (metav1.Condition, bool) {
	if apimeta.IsStatusConditionTrue(j.Status.Conditions, string(jobsetapi.JobSetCompleted)) {
		condition := metav1.Condition{
			Type:    kueue.WorkloadFinished,
			Status:  metav1.ConditionTrue,
			Reason:  "JobSetFinished",
			Message: "JobSet finished successfully",
		}
		return condition, true
	}
	if apimeta.IsStatusConditionTrue(j.Status.Conditions, string(jobsetapi.JobSetFailed)) {
		condition := metav1.Condition{
			Type:    kueue.WorkloadFinished,
			Status:  metav1.ConditionTrue,
			Reason:  "JobSetFinished",
			Message: "JobSet failed",
		}
		return condition, true
	}
	return metav1.Condition{}, false
}

func (j *JobSet) EquivalentToWorkload(wl kueue.Workload) bool {
	podSets := wl.Spec.PodSets
	if len(podSets) != len(j.Spec.ReplicatedJobs) {
		return false
	}

	for index := range j.Spec.ReplicatedJobs {
		if wl.Spec.PodSets[index].Count != podsCount(&j.Spec.ReplicatedJobs[index]) {
			return false
		}

		jobPodSpec := &j.Spec.ReplicatedJobs[index].Template.Spec.Template.Spec
		if !equality.Semantic.DeepEqual(jobPodSpec.InitContainers, podSets[index].Template.Spec.InitContainers) {
			return false
		}
		if !equality.Semantic.DeepEqual(jobPodSpec.Containers, podSets[index].Template.Spec.Containers) {
			return false
		}
	}
	return true
}

func (j *JobSet) PriorityClass() string {
	for _, replicatedJob := range j.Spec.ReplicatedJobs {
		if len(replicatedJob.Template.Spec.Template.Spec.PriorityClassName) != 0 {
			return replicatedJob.Template.Spec.Template.Spec.PriorityClassName
		}
	}
	return ""
}

func (j *JobSet) PodsReady() bool {
	var replicas int32
	for _, replicatedJob := range j.Spec.ReplicatedJobs {
		replicas += int32(replicatedJob.Replicas)
	}
	var jobsReady int32
	for _, replicatedJobStatus := range j.Status.ReplicatedJobsStatus {
		jobsReady += replicatedJobStatus.Ready + replicatedJobStatus.Succeeded
	}
	return replicas == jobsReady
}

func podsCount(rj *jobsetapi.ReplicatedJob) int32 {
	replicas := rj.Replicas
	job := rj.Template
	// parallelism is always set as it is otherwise defaulted by k8s to 1
	jobPodsCount := pointer.Int32Deref(job.Spec.Parallelism, 1)
	if comp := pointer.Int32Deref(job.Spec.Completions, jobPodsCount); comp < jobPodsCount {
		jobPodsCount = comp
	}
	return int32(replicas) * jobPodsCount
}

func SetupIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	return jobframework.SetupWorkloadOwnerIndex(ctx, indexer, gvk)
}

func GetWorkloadNameForJobSet(jobSetName string) string {
	return jobframework.GetWorkloadNameForOwnerWithGVK(jobSetName, gvk)
}
