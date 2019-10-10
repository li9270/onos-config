// Copyright 2019-present Open Networking Foundation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package network

import (
	"github.com/onosproject/onos-config/pkg/controller"
	devicechangestore "github.com/onosproject/onos-config/pkg/store/change/device"
	networkchangestore "github.com/onosproject/onos-config/pkg/store/change/network"
	devicestore "github.com/onosproject/onos-config/pkg/store/device"
	leadershipstore "github.com/onosproject/onos-config/pkg/store/leadership"
	"github.com/onosproject/onos-config/pkg/types"
	changetypes "github.com/onosproject/onos-config/pkg/types/change"
	devicetypes "github.com/onosproject/onos-config/pkg/types/change/device"
	networktypes "github.com/onosproject/onos-config/pkg/types/change/network"
)

// NewController returns a new config controller
func NewController(leadership leadershipstore.Store, deviceStore devicestore.Store, networkChanges networkchangestore.Store, deviceChanges devicechangestore.Store) *controller.Controller {
	c := controller.NewController()
	c.Activate(&controller.LeadershipActivator{
		Store: leadership,
	})
	c.Watch(&Watcher{
		Store: networkChanges,
	})
	c.Watch(&DeviceWatcher{
		DeviceStore: deviceStore,
		ChangeStore: deviceChanges,
	})
	c.Reconcile(&Reconciler{
		networkChanges: networkChanges,
		deviceChanges:  deviceChanges,
	})
	return c
}

// Reconciler is a config reconciler
type Reconciler struct {
	networkChanges networkchangestore.Store
	deviceChanges  devicechangestore.Store
	// changeIndex is the index of the highest sequential network change applied
	changeIndex networktypes.Index
}

// Reconcile reconciles the state of a network configuration
func (r *Reconciler) Reconcile(id types.ID) (bool, error) {
	change, err := r.networkChanges.Get(networktypes.ID(id))
	if err != nil {
		return false, err
	}

	// Handle the change for each phase
	if change != nil {
		// For all phases, ensure device changes have been created in the device change store
		succeeded, err := r.ensureDeviceChanges(change)
		if succeeded || err != nil {
			return succeeded, err
		}

		switch change.Status.Phase {
		case changetypes.Phase_CHANGE:
			return r.reconcileChange(change)
		case changetypes.Phase_ROLLBACK:
			return r.reconcileRollback(change)
		}
	}
	return true, nil
}

// ensureDeviceChanges ensures device changes have been created for all changes in the network change
func (r *Reconciler) ensureDeviceChanges(config *networktypes.NetworkChange) (bool, error) {
	// Loop through changes and create if necessary
	updated := false
	for _, change := range config.Changes {
		if change.ID == "" {
			deviceChange := &devicetypes.Change{
				NetworkChangeID: types.ID(config.ID),
				DeviceID:        change.DeviceID,
				DeviceVersion:   change.DeviceVersion,
				Values:          change.Values,
			}
			if err := r.deviceChanges.Create(deviceChange); err != nil {
				return false, err
			}
			change.ID = deviceChange.ID
			change.Index = deviceChange.Index
			updated = true
		}
	}

	// If indexes have been updated, store the indexes first in the network change
	if updated {
		if err := r.networkChanges.Update(config); err != nil {
			return false, err
		}
	}
	return updated, nil
}

// reconcileChange reconciles a change in the CHANGE phase
func (r *Reconciler) reconcileChange(change *networktypes.NetworkChange) (bool, error) {
	// Handle each possible state of the phase
	switch change.Status.State {
	case changetypes.State_PENDING:
		return r.reconcilePendingChange(change)
	case changetypes.State_RUNNING:
		return r.reconcileRunningChange(change)
	default:
		return true, nil
	}
}

// reconcilePendingChange reconciles a change in the PENDING state during the CHANGE phase
func (r *Reconciler) reconcilePendingChange(change *networktypes.NetworkChange) (bool, error) {
	// Determine whether the change can be applied
	canApply, err := r.canApplyChange(change)
	if err != nil {
		return false, err
	} else if !canApply {
		return false, nil
	}

	// If the change can be applied, update the change state to RUNNING
	change.Status.State = changetypes.State_RUNNING
	if err := r.networkChanges.Update(change); err != nil {
		return false, err
	}
	return true, nil
}

// canApplyChange returns a bool indicating whether the change can be applied
func (r *Reconciler) canApplyChange(change *networktypes.NetworkChange) (bool, error) {
	sequential := true
	for index := r.changeIndex; index < change.Index; index++ {
		priorChange, err := r.networkChanges.GetByIndex(index)
		if err != nil {
			return false, err
		} else if priorChange != nil {
			if priorChange.Status.State == changetypes.State_PENDING || priorChange.Status.State == changetypes.State_RUNNING {
				if isIntersectingChange(change, priorChange) {
					return false, nil
				}
				sequential = false
			} else {
				if sequential {
					r.changeIndex++
				}
			}
		}
	}
	return true, nil
}

// reconcileRunningChange reconciles a change in the RUNNING state during the CHANGE phase
func (r *Reconciler) reconcileRunningChange(change *networktypes.NetworkChange) (bool, error) {
	// Get the current state of all device changes for the change
	deviceChanges, err := r.getDeviceChanges(change)
	if err != nil {
		return false, err
	}

	// Ensure the device changes are being applied
	succeeded, err := r.ensureDeviceChangesRunning(deviceChanges)
	if succeeded || err != nil {
		return succeeded, err
	}

	// If all device changes are complete, mark the network change complete
	if r.isDeviceChangesComplete(deviceChanges) {
		change.Status.State = changetypes.State_COMPLETE
		if err := r.networkChanges.Update(change); err != nil {
			return false, err
		}
		return true, nil
	}

	// If a device change failed, rollback pending changes and requeue the change
	if r.isDeviceChangesFailed(deviceChanges) {
		// Ensure changes that have not failed are being rolled back
		succeeded, err = r.ensureDeviceChangeRollbacksRunning(deviceChanges)
		if succeeded || err != nil {
			return succeeded, err
		}

		// If all device change rollbacks have completed, revert the network change to PENDING
		if r.isDeviceChangeRollbacksComplete(deviceChanges) {
			change.Status.State = changetypes.State_PENDING
			change.Status.Reason = changetypes.Reason_ERROR
			if err := r.networkChanges.Update(change); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// ensureDeviceChangesRunning ensures device changes are in the running state
func (r *Reconciler) ensureDeviceChangesRunning(changes []*devicetypes.Change) (bool, error) {
	// Ensure all device changes are being applied
	updated := false
	for _, deviceChange := range changes {
		if deviceChange.Status.State == changetypes.State_PENDING {
			deviceChange.Status.State = changetypes.State_RUNNING
			if err := r.deviceChanges.Update(deviceChange); err != nil {
				return false, err
			}
			updated = true
		}
	}
	return updated, nil
}

// getDeviceChanges gets the device changes for the given network change
func (r *Reconciler) getDeviceChanges(change *networktypes.NetworkChange) ([]*devicetypes.Change, error) {
	deviceChanges := make([]*devicetypes.Change, len(change.Changes))
	for i, changeReq := range change.Changes {
		deviceChange, err := r.deviceChanges.Get(changeReq.ID)
		if err != nil {
			return nil, err
		}
		deviceChanges[i] = deviceChange
	}
	return deviceChanges, nil
}

// isDeviceChangesComplete checks whether the device changes are complete
func (r *Reconciler) isDeviceChangesComplete(changes []*devicetypes.Change) bool {
	for _, change := range changes {
		if change.Status.State != changetypes.State_COMPLETE {
			return false
		}
	}
	return true
}

// isDeviceChangesFailed checks whether the device changes are complete
func (r *Reconciler) isDeviceChangesFailed(changes []*devicetypes.Change) bool {
	for _, change := range changes {
		if change.Status.State == changetypes.State_FAILED {
			return true
		}
	}
	return false
}

// ensureDeviceChangeRollbacksRunning ensures RUNNING or COMPLETE device changes are being rolled back
func (r *Reconciler) ensureDeviceChangeRollbacksRunning(changes []*devicetypes.Change) (bool, error) {
	updated := false
	for _, deviceChange := range changes {
		if deviceChange.Status.Phase == changetypes.Phase_CHANGE && deviceChange.Status.State != changetypes.State_FAILED {
			deviceChange.Status.Phase = changetypes.Phase_ROLLBACK
			deviceChange.Status.State = changetypes.State_RUNNING
			if err := r.deviceChanges.Update(deviceChange); err != nil {
				return false, err
			}
			updated = true
		}
	}
	return updated, nil
}

// isDeviceChangeRollbacksComplete determines whether a rollback of device changes is complete
func (r *Reconciler) isDeviceChangeRollbacksComplete(changes []*devicetypes.Change) bool {
	for _, deviceChange := range changes {
		if deviceChange.Status.Phase == changetypes.Phase_ROLLBACK && deviceChange.Status.State != changetypes.State_COMPLETE {
			return false
		}
	}
	return true
}

// reconcileRollback reconciles a change in the ROLLBACK phase
func (r *Reconciler) reconcileRollback(change *networktypes.NetworkChange) (bool, error) {
	// Ensure the device changes are in the ROLLBACK phase
	updated, err := r.ensureDeviceRollbacks(change)
	if updated || err != nil {
		return updated, err
	}

	// Handle each possible state of the phase
	switch change.Status.State {
	case changetypes.State_PENDING:
		return r.reconcilePendingRollback(change)
	case changetypes.State_RUNNING:
		return r.reconcileRunningRollback(change)
	default:
		return true, nil
	}
}

// ensureDeviceRollbacks ensures device rollbacks are pending
func (r *Reconciler) ensureDeviceRollbacks(change *networktypes.NetworkChange) (bool, error) {
	// Ensure all device changes are being rolled back
	updated := false
	for _, changeReq := range change.Changes {
		deviceChange, err := r.deviceChanges.Get(changeReq.ID)
		if err != nil {
			return false, err
		}

		if deviceChange.Status.Phase != changetypes.Phase_ROLLBACK {
			deviceChange.Status.Phase = changetypes.Phase_ROLLBACK
			deviceChange.Status.State = changetypes.State_PENDING
			if err := r.deviceChanges.Update(deviceChange); err != nil {
				return false, err
			}
			updated = true
		}
	}
	return updated, nil
}

// reconcilePendingRollback reconciles a change in the PENDING state during the ROLLBACK phase
func (r *Reconciler) reconcilePendingRollback(change *networktypes.NetworkChange) (bool, error) {
	// Determine whether the rollback can be applied
	canApply, err := r.canApplyRollback(change)
	if err != nil {
		return false, err
	} else if !canApply {
		return false, nil
	}

	// If the rollback can be applied, update the change state to RUNNING
	change.Status.State = changetypes.State_RUNNING
	if err := r.networkChanges.Update(change); err != nil {
		return false, err
	}
	return true, nil
}

// canApplyRollback returns a bool indicating whether the rollback can be applied
func (r *Reconciler) canApplyRollback(change *networktypes.NetworkChange) (bool, error) {
	lastIndex, err := r.networkChanges.LastIndex()
	if err != nil {
		return false, err
	}

	for index := change.Index + 1; index <= lastIndex; index++ {
		futureChange, err := r.networkChanges.GetByIndex(index)
		if err != nil {
			return false, err
		} else if futureChange != nil && isIntersectingChange(change, futureChange) && futureChange.Status.State != changetypes.State_COMPLETE && futureChange.Status.State != changetypes.State_FAILED {
			return false, err
		}
	}
	return true, nil
}

// reconcileRunningRollback reconciles a change in the RUNNING state during the ROLLBACK phase
func (r *Reconciler) reconcileRunningRollback(change *networktypes.NetworkChange) (bool, error) {
	// Ensure the device rollbacks are running
	succeeded, err := r.ensureDeviceRollbacksRunning(change)
	if succeeded || err != nil {
		return succeeded, err
	}

	// If the rollback is complete, update the change state. Otherwise discard the change.
	complete, err := r.isRollbackComplete(change)
	if err != nil {
		return false, err
	} else if !complete {
		return true, nil
	}

	change.Status.State = changetypes.State_COMPLETE
	if err := r.networkChanges.Update(change); err != nil {
		return false, nil
	}
	return true, nil
}

// ensureDeviceRollbacksRunning ensures device rollbacks are in the running state
func (r *Reconciler) ensureDeviceRollbacksRunning(change *networktypes.NetworkChange) (bool, error) {
	updated := false
	for _, changeReq := range change.Changes {
		deviceChange, err := r.deviceChanges.Get(changeReq.ID)
		if err != nil {
			return false, err
		}

		if deviceChange.Status.State == changetypes.State_PENDING {
			deviceChange.Status.State = changetypes.State_RUNNING
			if err := r.deviceChanges.Update(deviceChange); err != nil {
				return false, err
			}
			updated = true
		}
	}
	return updated, nil
}

// isRollbackComplete determines whether a rollback is complete
func (r *Reconciler) isRollbackComplete(change *networktypes.NetworkChange) (bool, error) {
	complete := 0
	for _, changeReq := range change.Changes {
		deviceChange, err := r.deviceChanges.Get(changeReq.ID)
		if err != nil {
			return false, err
		}

		if deviceChange.Status.State == changetypes.State_COMPLETE {
			complete++
		}
	}
	return complete == len(change.Changes), nil
}

// isIntersectingChange indicates whether the changes from the two given NetworkChanges intersect
func isIntersectingChange(config *networktypes.NetworkChange, history *networktypes.NetworkChange) bool {
	for _, configChange := range config.Changes {
		for _, historyChange := range history.Changes {
			if configChange.DeviceID == historyChange.DeviceID {
				return true
			}
		}
	}
	return false
}

var _ controller.Reconciler = &Reconciler{}