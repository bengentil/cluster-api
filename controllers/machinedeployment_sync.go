/*
Copyright 2018 The Kubernetes Authors.

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
	"reflect"
	"sort"
	"strconv"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	apirand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha3"
	"sigs.k8s.io/cluster-api/controllers/mdutil"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// sync is responsible for reconciling deployments on scaling events or when they
// are paused.
func (r *MachineDeploymentReconciler) sync(d *clusterv1.MachineDeployment, msList []*clusterv1.MachineSet, machineMap map[types.UID]*clusterv1.MachineList) error {
	newMS, oldMSs, err := r.getAllMachineSetsAndSyncRevision(d, msList, machineMap, false)
	if err != nil {
		return err
	}

	if err := r.scale(d, newMS, oldMSs); err != nil {
		// If we get an error while trying to scale, the deployment will be requeued
		// so we can abort this resync
		return err
	}

	//
	// // TODO: Clean up the deployment when it's paused and no rollback is in flight.
	//
	allMSs := append(oldMSs, newMS)
	return r.syncDeploymentStatus(allMSs, newMS, d)
}

// getAllMachineSetsAndSyncRevision returns all the machine sets for the provided deployment (new and all old), with new MS's and deployment's revision updated.
//
// msList should come from getMachineSetsForDeployment(d).
// machineMap should come from getMachineMapForDeployment(d, msList).
//
// 1. Get all old MSes this deployment targets, and calculate the max revision number among them (maxOldV).
// 2. Get new MS this deployment targets (whose machine template matches deployment's), and update new MS's revision number to (maxOldV + 1),
//    only if its revision number is smaller than (maxOldV + 1). If this step failed, we'll update it in the next deployment sync loop.
// 3. Copy new MS's revision number to deployment (update deployment's revision). If this step failed, we'll update it in the next deployment sync loop.
//
// Note that currently the deployment controller is using caches to avoid querying the server for reads.
// This may lead to stale reads of machine sets, thus incorrect deployment status.
func (r *MachineDeploymentReconciler) getAllMachineSetsAndSyncRevision(d *clusterv1.MachineDeployment, msList []*clusterv1.MachineSet, machineMap map[types.UID]*clusterv1.MachineList, createIfNotExisted bool) (*clusterv1.MachineSet, []*clusterv1.MachineSet, error) {
	_, allOldMSs := mdutil.FindOldMachineSets(d, msList)

	// Get new machine set with the updated revision number
	newMS, err := r.getNewMachineSet(d, msList, allOldMSs, createIfNotExisted)
	if err != nil {
		return nil, nil, err
	}

	return newMS, allOldMSs, nil
}

// Returns a machine set that matches the intent of the given deployment. Returns nil if the new machine set doesn't exist yet.
// 1. Get existing new MS (the MS that the given deployment targets, whose machine template is the same as deployment's).
// 2. If there's existing new MS, update its revision number if it's smaller than (maxOldRevision + 1), where maxOldRevision is the max revision number among all old MSes.
// 3. If there's no existing new MS and createIfNotExisted is true, create one with appropriate revision number (maxOldRevision + 1) and replicas.
// Note that the machine-template-hash will be added to adopted MSes and machines.
func (r *MachineDeploymentReconciler) getNewMachineSet(d *clusterv1.MachineDeployment, msList, oldMSs []*clusterv1.MachineSet, createIfNotExisted bool) (*clusterv1.MachineSet, error) {
	logger := r.Log.WithValues("machinedeployment", d.Name, "namespace", d.Namespace)

	existingNewMS := mdutil.FindNewMachineSet(d, msList)

	// Calculate the max revision number among all old MSes
	maxOldRevision := mdutil.MaxRevision(oldMSs, logger)

	// Calculate revision number for this new machine set
	newRevision := strconv.FormatInt(maxOldRevision+1, 10)

	// Latest machine set exists. We need to sync its annotations (includes copying all but
	// annotationsToSkip from the parent deployment, and update revision, desiredReplicas,
	// and maxReplicas) and also update the revision annotation in the deployment with the
	// latest revision.
	if existingNewMS != nil {
		msCopy := existingNewMS.DeepCopy()
		patch := client.MergeFrom(msCopy.DeepCopy())

		// Set existing new machine set's annotation
		annotationsUpdated := mdutil.SetNewMachineSetAnnotations(d, msCopy, newRevision, true, logger)

		minReadySecondsNeedsUpdate := msCopy.Spec.MinReadySeconds != *d.Spec.MinReadySeconds
		if annotationsUpdated || minReadySecondsNeedsUpdate {
			msCopy.Spec.MinReadySeconds = *d.Spec.MinReadySeconds
			return nil, r.Client.Patch(context.Background(), msCopy, patch)
		}

		// Apply revision annotation from existingNewMS if it is missing from the deployment.
		err := r.updateMachineDeployment(d, func(innerDeployment *clusterv1.MachineDeployment) {
			mdutil.SetDeploymentRevision(d, msCopy.Annotations[mdutil.RevisionAnnotation])
		})
		return msCopy, err
	}

	if !createIfNotExisted {
		return nil, nil
	}

	// new MachineSet does not exist, create one.
	newMSTemplate := *d.Spec.Template.DeepCopy()
	machineTemplateSpecHash := fmt.Sprintf("%d", mdutil.ComputeHash(&newMSTemplate))
	newMSTemplate.Labels = mdutil.CloneAndAddLabel(d.Spec.Template.Labels,
		mdutil.DefaultMachineDeploymentUniqueLabelKey, machineTemplateSpecHash)

	// Add machineTemplateHash label to selector.
	newMSSelector := mdutil.CloneSelectorAndAddLabel(&d.Spec.Selector,
		mdutil.DefaultMachineDeploymentUniqueLabelKey, machineTemplateSpecHash)

	minReadySeconds := int32(0)
	if d.Spec.MinReadySeconds != nil {
		minReadySeconds = *d.Spec.MinReadySeconds
	}

	// Create new MachineSet
	newMS := clusterv1.MachineSet{
		ObjectMeta: metav1.ObjectMeta{
			// Make the name deterministic, to ensure idempotence
			Name:            d.Name + "-" + apirand.SafeEncodeString(machineTemplateSpecHash),
			Namespace:       d.Namespace,
			Labels:          newMSTemplate.Labels,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(d, machineDeploymentKind)},
		},
		Spec: clusterv1.MachineSetSpec{
			ClusterName:     d.Spec.ClusterName,
			Replicas:        new(int32),
			MinReadySeconds: minReadySeconds,
			Selector:        *newMSSelector,
			Template:        newMSTemplate,
		},
	}

	// Add foregroundDeletion finalizer to MachineSet if the MachineDeployment has it
	if sets.NewString(d.Finalizers...).Has(metav1.FinalizerDeleteDependents) {
		newMS.Finalizers = []string{metav1.FinalizerDeleteDependents}
	}

	allMSs := append(oldMSs, &newMS)
	newReplicasCount, err := mdutil.NewMSNewReplicas(d, allMSs, &newMS)
	if err != nil {
		return nil, err
	}

	*(newMS.Spec.Replicas) = newReplicasCount

	// Set new machine set's annotation
	mdutil.SetNewMachineSetAnnotations(d, &newMS, newRevision, false, logger)
	// Create the new MachineSet. If it already exists, then we need to check for possible
	// hash collisions. If there is any other error, we need to report it in the status of
	// the Deployment.
	alreadyExists := false
	err = r.Client.Create(context.Background(), &newMS)
	createdMS := &newMS
	switch {
	// We may end up hitting this due to a slow cache or a fast resync of the Deployment.
	case apierrors.IsAlreadyExists(err):
		alreadyExists = true

		ms := &clusterv1.MachineSet{}
		msErr := r.Client.Get(context.Background(), client.ObjectKey{Namespace: newMS.Namespace, Name: newMS.Name}, ms)
		if msErr != nil {
			return nil, msErr
		}

		// If the Deployment owns the MachineSet and the MachineSet's MachineTemplateSpec is semantically
		// deep equal to the MachineTemplateSpec of the Deployment, it's the Deployment's new MachineSet.
		// Otherwise, this is a hash collision and we need to increment the collisionCount field in
		// the status of the Deployment and requeue to try the creation in the next sync.
		controllerRef := metav1.GetControllerOf(ms)
		if controllerRef != nil && controllerRef.UID == d.UID && mdutil.EqualIgnoreHash(&d.Spec.Template, &ms.Spec.Template) {
			createdMS = ms
			break
		}

		return nil, err
	case err != nil:
		logger.Error(err, "Failed to create new machine set", "machineset", newMS.Name)
		r.recorder.Eventf(d, corev1.EventTypeWarning, "FailedCreate", "Failed to create MachineSet %q: %v", newMS.Name, err)
		return nil, err
	}

	if !alreadyExists {
		logger.V(4).Info("Created new machine set", "machineset", createdMS.Name)
		r.recorder.Eventf(d, corev1.EventTypeNormal, "SuccessfulCreate", "Created MachineSet %q", newMS.Name)
	}

	err = r.updateMachineDeployment(d, func(innerDeployment *clusterv1.MachineDeployment) {
		mdutil.SetDeploymentRevision(d, newRevision)
	})

	return createdMS, err
}

// scale scales proportionally in order to mitigate risk. Otherwise, scaling up can increase the size
// of the new machine set and scaling down can decrease the sizes of the old ones, both of which would
// have the effect of hastening the rollout progress, which could produce a higher proportion of unavailable
// replicas in the event of a problem with the rolled out template. Should run only on scaling events or
// when a deployment is paused and not during the normal rollout process.
func (r *MachineDeploymentReconciler) scale(deployment *clusterv1.MachineDeployment, newMS *clusterv1.MachineSet, oldMSs []*clusterv1.MachineSet) error {
	logger := r.Log.WithValues("machinedeployment", deployment.Name, "namespace", deployment.Namespace)

	if deployment.Spec.Replicas == nil {
		return errors.Errorf("spec replicas for deployment %v is nil, this is unexpected", deployment.Name)
	}

	// If there is only one active machine set then we should scale that up to the full count of the
	// deployment. If there is no active machine set, then we should scale up the newest machine set.
	if activeOrLatest := mdutil.FindOneActiveOrLatest(newMS, oldMSs); activeOrLatest != nil {
		if activeOrLatest.Spec.Replicas == nil {
			return errors.Errorf("spec replicas for machine set %v is nil, this is unexpected", activeOrLatest.Name)
		}

		if *(activeOrLatest.Spec.Replicas) == *(deployment.Spec.Replicas) {
			return nil
		}

		_, err := r.scaleMachineSet(activeOrLatest, *(deployment.Spec.Replicas), deployment)
		return err
	}

	// If the new machine set is saturated, old machine sets should be fully scaled down.
	// This case handles machine set adoption during a saturated new machine set.
	if mdutil.IsSaturated(deployment, newMS) {
		for _, old := range mdutil.FilterActiveMachineSets(oldMSs) {
			if _, err := r.scaleMachineSet(old, 0, deployment); err != nil {
				return err
			}
		}
		return nil
	}

	// There are old machine sets with machines and the new machine set is not saturated.
	// We need to proportionally scale all machine sets (new and old) in case of a
	// rolling deployment.
	if mdutil.IsRollingUpdate(deployment) {
		allMSs := mdutil.FilterActiveMachineSets(append(oldMSs, newMS))
		totalMSReplicas := mdutil.GetReplicaCountForMachineSets(allMSs)

		allowedSize := int32(0)
		if *(deployment.Spec.Replicas) > 0 {
			allowedSize = *(deployment.Spec.Replicas) + mdutil.MaxSurge(*deployment)
		}

		// Number of additional replicas that can be either added or removed from the total
		// replicas count. These replicas should be distributed proportionally to the active
		// machine sets.
		deploymentReplicasToAdd := allowedSize - totalMSReplicas

		// The additional replicas should be distributed proportionally amongst the active
		// machine sets from the larger to the smaller in size machine set. Scaling direction
		// drives what happens in case we are trying to scale machine sets of the same size.
		// In such a case when scaling up, we should scale up newer machine sets first, and
		// when scaling down, we should scale down older machine sets first.
		var scalingOperation string
		switch {
		case deploymentReplicasToAdd > 0:
			sort.Sort(mdutil.MachineSetsBySizeNewer(allMSs))
			scalingOperation = "up"
		case deploymentReplicasToAdd < 0:
			sort.Sort(mdutil.MachineSetsBySizeOlder(allMSs))
			scalingOperation = "down"
		}

		// Iterate over all active machine sets and estimate proportions for each of them.
		// The absolute value of deploymentReplicasAdded should never exceed the absolute
		// value of deploymentReplicasToAdd.
		deploymentReplicasAdded := int32(0)
		nameToSize := make(map[string]int32)
		for i := range allMSs {
			ms := allMSs[i]
			if ms.Spec.Replicas == nil {
				logger.Info("Spec.Replicas for machine set is nil, this is unexpected.", "machineset", ms.Name)
				continue
			}

			// Estimate proportions if we have replicas to add, otherwise simply populate
			// nameToSize with the current sizes for each machine set.
			if deploymentReplicasToAdd != 0 {
				proportion := mdutil.GetProportion(ms, *deployment, deploymentReplicasToAdd, deploymentReplicasAdded, logger)
				nameToSize[ms.Name] = *(ms.Spec.Replicas) + proportion
				deploymentReplicasAdded += proportion
			} else {
				nameToSize[ms.Name] = *(ms.Spec.Replicas)
			}
		}

		// Update all machine sets
		for i := range allMSs {
			ms := allMSs[i]

			// Add/remove any leftovers to the largest machine set.
			if i == 0 && deploymentReplicasToAdd != 0 {
				leftover := deploymentReplicasToAdd - deploymentReplicasAdded
				nameToSize[ms.Name] = nameToSize[ms.Name] + leftover
				if nameToSize[ms.Name] < 0 {
					nameToSize[ms.Name] = 0
				}
			}

			// TODO: Use transactions when we have them.
			if _, err := r.scaleMachineSetOperation(ms, nameToSize[ms.Name], deployment, scalingOperation); err != nil {
				// Return as soon as we fail, the deployment is requeued
				return err
			}
		}
	}

	return nil
}

// syncDeploymentStatus checks if the status is up-to-date and sync it if necessary
func (r *MachineDeploymentReconciler) syncDeploymentStatus(allMSs []*clusterv1.MachineSet, newMS *clusterv1.MachineSet, d *clusterv1.MachineDeployment) error {
	newStatus := calculateStatus(allMSs, newMS, d)
	if reflect.DeepEqual(d.Status, newStatus) {
		return nil
	}

	patch := client.MergeFrom(d.DeepCopy())
	d.Status = newStatus
	// Patch using a deep copy to avoid overwriting any unexpected Spec/Metadata changes from the returned result
	return r.Client.Status().Patch(context.Background(), d.DeepCopy(), patch)
}

// calculateStatus calculates the latest status for the provided deployment by looking into the provided machine sets.
func calculateStatus(allMSs []*clusterv1.MachineSet, newMS *clusterv1.MachineSet, deployment *clusterv1.MachineDeployment) clusterv1.MachineDeploymentStatus {
	availableReplicas := mdutil.GetAvailableReplicaCountForMachineSets(allMSs)
	totalReplicas := mdutil.GetReplicaCountForMachineSets(allMSs)
	unavailableReplicas := totalReplicas - availableReplicas

	// If unavailableReplicas is negative, then that means the Deployment has more available replicas running than
	// desired, e.g. whenever it scales down. In such a case we should simply default unavailableReplicas to zero.
	if unavailableReplicas < 0 {
		unavailableReplicas = 0
	}

	status := clusterv1.MachineDeploymentStatus{
		// TODO: Ensure that if we start retrying status updates, we won't pick up a new Generation value.
		ObservedGeneration:  deployment.Generation,
		Replicas:            mdutil.GetActualReplicaCountForMachineSets(allMSs),
		UpdatedReplicas:     mdutil.GetActualReplicaCountForMachineSets([]*clusterv1.MachineSet{newMS}),
		ReadyReplicas:       mdutil.GetReadyReplicaCountForMachineSets(allMSs),
		AvailableReplicas:   availableReplicas,
		UnavailableReplicas: unavailableReplicas,
	}

	return status
}

func (r *MachineDeploymentReconciler) scaleMachineSet(ms *clusterv1.MachineSet, newScale int32, deployment *clusterv1.MachineDeployment) (bool, error) {
	if ms.Spec.Replicas == nil {
		return false, errors.Errorf("spec replicas for machine set %v is nil, this is unexpected", ms.Name)
	}

	// No need to scale
	if *(ms.Spec.Replicas) == newScale {
		return false, nil
	}

	var scalingOperation string
	if *(ms.Spec.Replicas) < newScale {
		scalingOperation = "up"
	} else {
		scalingOperation = "down"
	}

	return r.scaleMachineSetOperation(ms, newScale, deployment, scalingOperation)
}

func (r *MachineDeploymentReconciler) scaleMachineSetOperation(ms *clusterv1.MachineSet, newScale int32, deployment *clusterv1.MachineDeployment, scaleOperation string) (bool, error) {
	if ms.Spec.Replicas == nil {
		return false, errors.Errorf("spec replicas for machine set %v is nil, this is unexpected", ms.Name)
	}

	sizeNeedsUpdate := *(ms.Spec.Replicas) != newScale

	annotationsNeedUpdate := mdutil.ReplicasAnnotationsNeedUpdate(
		ms,
		*(deployment.Spec.Replicas),
		*(deployment.Spec.Replicas)+mdutil.MaxSurge(*deployment),
	)

	var (
		scaled bool
		err    error
	)

	if sizeNeedsUpdate || annotationsNeedUpdate {
		patch := client.MergeFrom(ms.DeepCopy())

		*(ms.Spec.Replicas) = newScale
		mdutil.SetReplicasAnnotations(ms, *(deployment.Spec.Replicas), *(deployment.Spec.Replicas)+mdutil.MaxSurge(*deployment))

		err = r.Client.Patch(context.Background(), ms, patch)
		if err != nil {
			r.recorder.Eventf(deployment, corev1.EventTypeWarning, "FailedScale", "Failed to scale MachineSet %q: %v", ms.Name, err)
		} else if sizeNeedsUpdate {
			scaled = true
			r.recorder.Eventf(deployment, corev1.EventTypeNormal, "SuccessfulScale", "Scaled %s MachineSet %q to %d", scaleOperation, ms.Name, newScale)
		}
	}

	return scaled, err
}

// cleanupDeployment is responsible for cleaning up a deployment i.e. retains all but the latest N old machine sets
// where N=d.Spec.RevisionHistoryLimit. Old machine sets are older versions of the machinetemplate of a deployment kept
// around by default 1) for historical reasons and 2) for the ability to rollback a deployment.
func (r *MachineDeploymentReconciler) cleanupDeployment(oldMSs []*clusterv1.MachineSet, deployment *clusterv1.MachineDeployment) error {
	logger := r.Log.WithValues("machinedeployment", deployment.Name, "namespace", deployment.Namespace)

	if deployment.Spec.RevisionHistoryLimit == nil {
		return nil
	}

	// Avoid deleting machine set with deletion timestamp set
	aliveFilter := func(ms *clusterv1.MachineSet) bool {
		return ms != nil && ms.ObjectMeta.DeletionTimestamp == nil
	}

	cleanableMSes := mdutil.FilterMachineSets(oldMSs, aliveFilter)

	diff := int32(len(cleanableMSes)) - *deployment.Spec.RevisionHistoryLimit
	if diff <= 0 {
		return nil
	}

	sort.Sort(mdutil.MachineSetsByCreationTimestamp(cleanableMSes))
	logger.V(4).Info("Looking to cleanup old machine sets for deployment")

	for i := int32(0); i < diff; i++ {
		ms := cleanableMSes[i]
		if ms.Spec.Replicas == nil {
			return errors.Errorf("spec replicas for machine set %v is nil, this is unexpected", ms.Name)
		}

		// Avoid delete machine set with non-zero replica counts
		if ms.Status.Replicas != 0 || *(ms.Spec.Replicas) != 0 || ms.Generation > ms.Status.ObservedGeneration || ms.DeletionTimestamp != nil {
			continue
		}

		logger.V(4).Info("Trying to cleanup machine set for deployment", "machineset", ms.Name)
		if err := r.Client.Delete(context.Background(), ms); err != nil && !apierrors.IsNotFound(err) {
			// Return error instead of aggregating and continuing DELETEs on the theory
			// that we may be overloading the api server.
			r.recorder.Eventf(deployment, corev1.EventTypeWarning, "FailedDelete", "Failed to delete MachineSet %q: %v", ms.Name, err)
			return err
		}
		r.recorder.Eventf(deployment, corev1.EventTypeNormal, "SuccessfulDelete", "Deleted MachineSet %q", ms.Name)
	}

	return nil
}

func (r *MachineDeploymentReconciler) updateMachineDeployment(d *clusterv1.MachineDeployment, modify func(*clusterv1.MachineDeployment)) error {
	return updateMachineDeployment(r.Client, d, modify)
}

// We have this as standalone variant to be able to use it from the tests
func updateMachineDeployment(c client.Client, d *clusterv1.MachineDeployment, modify func(*clusterv1.MachineDeployment)) error {
	dCopy := d.DeepCopy()
	modify(dCopy)
	if equality.Semantic.DeepEqual(dCopy, d) {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// Get latest version.
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: d.Namespace, Name: d.Name}, d); err != nil {
			return err
		}
		// Save patch.
		patch := client.MergeFrom(d.DeepCopy())
		// Apply defaults.
		clusterv1.PopulateDefaultsMachineDeployment(d)
		// Apply modifications.
		modify(d)
		// Patch using a deep copy to avoid overwriting any unexpected Spec/Metadata changes from the returned result
		return c.Patch(context.Background(), d.DeepCopy(), patch)
	})
}
