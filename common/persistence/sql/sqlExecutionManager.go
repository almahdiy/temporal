// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package sql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"go.temporal.io/api/serviceerror"

	enumsspb "go.temporal.io/server/api/enums/v1"
	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/collection"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/persistence"
	p "go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/persistence/serialization"
	"go.temporal.io/server/common/persistence/sql/sqlplugin"
	"go.temporal.io/server/common/primitives"
)

type sqlExecutionManager struct {
	sqlStore
	shardID int32
}

var _ p.ExecutionStore = (*sqlExecutionManager)(nil)

// NewSQLExecutionStore creates an instance of ExecutionStore
func NewSQLExecutionStore(
	db sqlplugin.DB,
	logger log.Logger,
	shardID int32,
) (p.ExecutionStore, error) {

	return &sqlExecutionManager{
		shardID: shardID,
		sqlStore: sqlStore{
			db:     db,
			logger: logger,
		},
	}, nil
}

// txExecuteShardLocked executes f under transaction and with read lock on shard row
func (m *sqlExecutionManager) txExecuteShardLocked(
	ctx context.Context,
	operation string,
	rangeID int64,
	fn func(tx sqlplugin.Tx) error,
) error {

	return m.txExecute(ctx, operation, func(tx sqlplugin.Tx) error {
		if err := readLockShard(ctx, tx, m.shardID, rangeID); err != nil {
			return err
		}
		err := fn(tx)
		if err != nil {
			return err
		}
		return nil
	})
}

func (m *sqlExecutionManager) GetShardID() int32 {
	return m.shardID
}

func (m *sqlExecutionManager) CreateWorkflowExecution(
	request *p.InternalCreateWorkflowExecutionRequest,
) (response *p.CreateWorkflowExecutionResponse, err error) {
	ctx, cancel := newExecutionContext()
	defer cancel()
	err = m.txExecuteShardLocked(ctx,
		"CreateWorkflowExecution",
		request.RangeID,
		func(tx sqlplugin.Tx) error {
			response, err = m.createWorkflowExecutionTx(ctx, tx, request)
			return err
		})
	return
}

func (m *sqlExecutionManager) createWorkflowExecutionTx(
	ctx context.Context,
	tx sqlplugin.Tx,
	request *p.InternalCreateWorkflowExecutionRequest,
) (*p.CreateWorkflowExecutionResponse, error) {

	newWorkflow := request.NewWorkflowSnapshot
	lastWriteVersion := newWorkflow.LastWriteVersion
	shardID := m.shardID
	namespaceID := primitives.MustParseUUID(newWorkflow.ExecutionInfo.NamespaceId)
	workflowID := newWorkflow.ExecutionInfo.WorkflowId
	runID := primitives.MustParseUUID(newWorkflow.ExecutionState.RunId)

	if err := p.ValidateCreateWorkflowModeState(
		request.Mode,
		newWorkflow,
	); err != nil {
		return nil, err
	}

	var err error
	var row *sqlplugin.CurrentExecutionsRow
	if row, err = lockCurrentExecutionIfExists(ctx,
		tx,
		m.shardID,
		namespaceID,
		workflowID,
	); err != nil {
		return nil, err
	}

	// current workflow record check
	if row != nil {
		// current run ID, last write version, current workflow state check
		switch request.Mode {
		case p.CreateWorkflowModeBrandNew:
			return nil, &p.WorkflowExecutionAlreadyStartedError{
				Msg:              fmt.Sprintf("Workflow execution already running. WorkflowId: %v", row.WorkflowID),
				StartRequestID:   row.CreateRequestID,
				RunID:            row.RunID.String(),
				State:            row.State,
				Status:           row.Status,
				LastWriteVersion: row.LastWriteVersion,
			}

		case p.CreateWorkflowModeWorkflowIDReuse:
			if request.PreviousLastWriteVersion != row.LastWriteVersion {
				return nil, &p.CurrentWorkflowConditionFailedError{
					Msg: fmt.Sprintf("Workflow execution creation condition failed. WorkflowId: %v, "+
						"LastWriteVersion: %v, PreviousLastWriteVersion: %v",
						workflowID, row.LastWriteVersion, request.PreviousLastWriteVersion),
				}
			}
			if row.State != enumsspb.WORKFLOW_EXECUTION_STATE_COMPLETED {
				return nil, &p.CurrentWorkflowConditionFailedError{
					Msg: fmt.Sprintf("Workflow execution creation condition failed. WorkflowId: %v, "+
						"State: %v, Expected: %v",
						workflowID, row.State, enumsspb.WORKFLOW_EXECUTION_STATE_COMPLETED),
				}
			}
			runIDStr := row.RunID.String()
			if runIDStr != request.PreviousRunID {
				return nil, &p.CurrentWorkflowConditionFailedError{
					Msg: fmt.Sprintf("Workflow execution creation condition failed. WorkflowId: %v, "+
						"RunId: %v, PreviousRunId: %v",
						workflowID, runIDStr, request.PreviousRunID),
				}
			}

		case p.CreateWorkflowModeZombie:
			// zombie workflow creation with existence of current record, this is a noop
			if err := assertRunIDMismatch(primitives.MustParseUUID(newWorkflow.ExecutionState.RunId), row.RunID); err != nil {
				return nil, err
			}

		case p.CreateWorkflowModeContinueAsNew:
			runIDStr := row.RunID.String()
			if runIDStr != request.PreviousRunID {
				return nil, &p.CurrentWorkflowConditionFailedError{
					Msg: fmt.Sprintf("Workflow execution creation condition failed. WorkflowId: %v, "+
						"RunId: %v, PreviousRunId: %v",
						workflowID, runIDStr, request.PreviousRunID),
				}
			}

		default:
			return nil, serviceerror.NewInternal(fmt.Sprintf("CreteWorkflowExecution: unknown mode: %v", request.Mode))
		}
	}

	if err := createOrUpdateCurrentExecution(ctx,
		tx,
		request.Mode,
		m.shardID,
		namespaceID,
		workflowID,
		runID,
		newWorkflow.ExecutionState.State,
		newWorkflow.ExecutionState.Status,
		newWorkflow.ExecutionState.CreateRequestId,
		newWorkflow.ExecutionInfo.StartVersion,
		lastWriteVersion); err != nil {
		return nil, err
	}

	if err := m.applyWorkflowSnapshotTxAsNew(ctx,
		tx,
		shardID,
		&request.NewWorkflowSnapshot,
	); err != nil {
		return nil, err
	}

	return &p.CreateWorkflowExecutionResponse{}, nil
}

func (m *sqlExecutionManager) GetWorkflowExecution(
	request *p.GetWorkflowExecutionRequest,
) (*p.InternalGetWorkflowExecutionResponse, error) {
	ctx, cancel := newExecutionContext()
	defer cancel()
	namespaceID := primitives.MustParseUUID(request.NamespaceID)
	runID := primitives.MustParseUUID(request.Execution.RunId)
	wfID := request.Execution.WorkflowId
	executionsRow, err := m.db.SelectFromExecutions(ctx, sqlplugin.ExecutionsFilter{
		ShardID: m.shardID, NamespaceID: namespaceID, WorkflowID: wfID, RunID: runID,
	})
	switch err {
	case nil:
		// noop
	case sql.ErrNoRows:
		return nil, serviceerror.NewNotFound(fmt.Sprintf("Workflow executionsRow not found.  WorkflowId: %v, RunId: %v", request.Execution.GetWorkflowId(), request.Execution.GetRunId()))
	default:
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetWorkflowExecution: failed. Error: %v", err))
	}

	info, err := serialization.WorkflowExecutionInfoFromBlob(executionsRow.Data, executionsRow.DataEncoding)
	if err != nil {
		return nil, err
	}

	executionState, err := serialization.WorkflowExecutionStateFromBlob(executionsRow.State, executionsRow.StateEncoding)
	if err != nil {
		return nil, err
	}

	state := &p.InternalWorkflowMutableState{
		ExecutionInfo:  info,
		ExecutionState: executionState,
		NextEventID:    executionsRow.NextEventID}

	state.ActivityInfos, err = getActivityInfoMap(ctx,
		m.db,
		m.shardID,
		namespaceID,
		wfID,
		runID,
	)
	if err != nil {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetWorkflowExecution: failed to get activity info. Error: %v", err))
	}

	state.TimerInfos, err = getTimerInfoMap(ctx,
		m.db,
		m.shardID,
		namespaceID,
		wfID,
		runID,
	)
	if err != nil {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetWorkflowExecution: failed to get timer info. Error: %v", err))
	}

	state.ChildExecutionInfos, err = getChildExecutionInfoMap(ctx,
		m.db,
		m.shardID,
		namespaceID,
		wfID,
		runID,
	)
	if err != nil {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetWorkflowExecution: failed to get child executionsRow info. Error: %v", err))
	}

	state.RequestCancelInfos, err = getRequestCancelInfoMap(ctx,
		m.db,
		m.shardID,
		namespaceID,
		wfID,
		runID,
	)
	if err != nil {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetWorkflowExecution: failed to get request cancel info. Error: %v", err))
	}

	state.SignalInfos, err = getSignalInfoMap(ctx,
		m.db,
		m.shardID,
		namespaceID,
		wfID,
		runID,
	)
	if err != nil {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetWorkflowExecution: failed to get signal info. Error: %v", err))
	}

	state.BufferedEvents, err = getBufferedEvents(ctx,
		m.db,
		m.shardID,
		namespaceID,
		wfID,
		runID,
	)
	if err != nil {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetWorkflowExecution: failed to get buffered events. Error: %v", err))
	}

	state.SignalRequestedIDs, err = getSignalsRequested(ctx,
		m.db,
		m.shardID,
		namespaceID,
		wfID,
		runID,
	)
	if err != nil {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetWorkflowExecution: failed to get signals requested. Error: %v", err))
	}

	return &p.InternalGetWorkflowExecutionResponse{State: state}, nil
}

func (m *sqlExecutionManager) UpdateWorkflowExecution(
	request *p.InternalUpdateWorkflowExecutionRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	return m.txExecuteShardLocked(ctx,
		"UpdateWorkflowExecution",
		request.RangeID,
		func(tx sqlplugin.Tx) error {
			return m.updateWorkflowExecutionTx(ctx, tx, request)
		})
}

func (m *sqlExecutionManager) updateWorkflowExecutionTx(
	ctx context.Context,
	tx sqlplugin.Tx,
	request *p.InternalUpdateWorkflowExecutionRequest,
) error {

	updateWorkflow := request.UpdateWorkflowMutation
	newWorkflow := request.NewWorkflowSnapshot

	namespaceID := primitives.MustParseUUID(updateWorkflow.ExecutionInfo.NamespaceId)
	workflowID := updateWorkflow.ExecutionInfo.WorkflowId
	runID := primitives.MustParseUUID(updateWorkflow.ExecutionState.RunId)
	shardID := m.shardID

	if err := p.ValidateUpdateWorkflowModeState(
		request.Mode,
		updateWorkflow,
		newWorkflow,
	); err != nil {
		return err
	}

	switch request.Mode {
	case p.UpdateWorkflowModeBypassCurrent:
		// do nothing
	case p.UpdateWorkflowModeUpdateCurrent:
		if newWorkflow != nil {
			lastWriteVersion := newWorkflow.LastWriteVersion
			newNamespaceID := primitives.MustParseUUID(newWorkflow.ExecutionInfo.NamespaceId)
			newRunID := primitives.MustParseUUID(newWorkflow.ExecutionState.RunId)

			if !bytes.Equal(namespaceID, newNamespaceID) {
				return serviceerror.NewInternal(fmt.Sprintf("UpdateWorkflowExecution: cannot continue as new to another namespace"))
			}

			if err := assertRunIDAndUpdateCurrentExecution(ctx,
				tx,
				shardID,
				namespaceID,
				workflowID,
				newRunID,
				runID,
				newWorkflow.ExecutionState.CreateRequestId,
				newWorkflow.ExecutionState.State,
				newWorkflow.ExecutionState.Status,
				newWorkflow.ExecutionInfo.StartVersion,
				lastWriteVersion,
			); err != nil {
				return serviceerror.NewInternal(fmt.Sprintf("UpdateWorkflowExecution: failed to continue as new current execution. Error: %v", err))
			}
		} else {
			lastWriteVersion := updateWorkflow.LastWriteVersion
			// this is only to update the current record
			if err := assertRunIDAndUpdateCurrentExecution(ctx,
				tx,
				shardID,
				namespaceID,
				workflowID,
				runID,
				runID,
				updateWorkflow.ExecutionState.CreateRequestId,
				updateWorkflow.ExecutionState.State,
				updateWorkflow.ExecutionState.Status,
				updateWorkflow.ExecutionInfo.StartVersion,
				lastWriteVersion,
			); err != nil {
				return serviceerror.NewInternal(fmt.Sprintf("UpdateWorkflowExecution: failed to update current execution. Error: %v", err))
			}
		}

	default:
		return serviceerror.NewInternal(fmt.Sprintf("UpdateWorkflowExecution: unknown mode: %v", request.Mode))
	}

	if err := applyWorkflowMutationTx(ctx,
		tx,
		shardID,
		&updateWorkflow,
	); err != nil {
		return err
	}

	if newWorkflow != nil {
		if err := m.applyWorkflowSnapshotTxAsNew(ctx,
			tx,
			shardID,
			newWorkflow,
		); err != nil {
			return err
		}
	}
	return nil
}

func (m *sqlExecutionManager) ResetWorkflowExecution(
	request *p.InternalResetWorkflowExecutionRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	return m.txExecuteShardLocked(ctx,
		"ResetWorkflowExecution",
		request.RangeID,
		func(tx sqlplugin.Tx) error {
			return m.resetWorkflowExecutionTx(ctx, tx, request)
		})
}

func (m *sqlExecutionManager) resetWorkflowExecutionTx(
	ctx context.Context,
	tx sqlplugin.Tx,
	request *p.InternalResetWorkflowExecutionRequest,
) error {

	shardID := m.shardID

	namespaceID := primitives.MustParseUUID(request.NewWorkflowSnapshot.ExecutionInfo.NamespaceId)
	workflowID := request.NewWorkflowSnapshot.ExecutionInfo.WorkflowId

	baseRunID := primitives.MustParseUUID(request.BaseRunID)
	baseRunNextEventID := request.BaseRunNextEventID

	currentRunID := primitives.MustParseUUID(request.CurrentRunID)
	currentRunNextEventID := request.CurrentRunNextEventID

	newWorkflowRunID := primitives.MustParseUUID(request.NewWorkflowSnapshot.ExecutionState.RunId)
	lastWriteVersion := request.NewWorkflowSnapshot.LastWriteVersion

	// 1. update current execution
	if err := updateCurrentExecution(ctx,
		tx,
		shardID,
		namespaceID,
		workflowID,
		newWorkflowRunID,
		request.NewWorkflowSnapshot.ExecutionState.CreateRequestId,
		request.NewWorkflowSnapshot.ExecutionState.State,
		request.NewWorkflowSnapshot.ExecutionState.Status,
		request.NewWorkflowSnapshot.ExecutionInfo.StartVersion,
		lastWriteVersion,
	); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("ResetWorkflowExecution operation failed. Failed at updateCurrentExecution. Error: %v", err))
	}

	// 2. lock base run: we want to grab a read-lock for base run to prevent race condition
	// It is only needed when base run is not current run. Because we will obtain a lock on current run anyway.
	if !bytes.Equal(baseRunID, currentRunID) {
		if err := lockAndCheckNextEventID(ctx,
			tx,
			shardID,
			namespaceID,
			workflowID,
			baseRunID,
			baseRunNextEventID,
		); err != nil {
			switch err.(type) {
			case *p.ConditionFailedError:
				return err
			default:
				return serviceerror.NewInternal(fmt.Sprintf("ResetWorkflowExecution operation failed. Failed to lock executions row. Error: %v", err))
			}
		}
	}

	// 3. update or lock current run
	if request.CurrentWorkflowMutation != nil {
		if err := applyWorkflowMutationTx(ctx,
			tx,
			m.shardID,
			request.CurrentWorkflowMutation,
		); err != nil {
			return err
		}
	} else {
		// even the current run is not running, we need to lock the current run:
		// 1). in case it is changed by conflict resolution
		// 2). in case delete history timer kicks in if the base is current
		if err := lockAndCheckNextEventID(ctx,
			tx,
			shardID,
			namespaceID,
			workflowID,
			currentRunID,
			currentRunNextEventID,
		); err != nil {
			switch err.(type) {
			case *p.ConditionFailedError:
				return err
			default:
				return serviceerror.NewInternal(fmt.Sprintf("ResetWorkflowExecution operation failed. Failed to lock executions row. Error: %v", err))
			}
		}
	}

	// 4. create the new reset workflow
	return m.applyWorkflowSnapshotTxAsNew(ctx,
		tx,
		m.shardID,
		&request.NewWorkflowSnapshot,
	)
}

func (m *sqlExecutionManager) ConflictResolveWorkflowExecution(
	request *p.InternalConflictResolveWorkflowExecutionRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	return m.txExecuteShardLocked(ctx,
		"ConflictResolveWorkflowExecution",
		request.RangeID,
		func(tx sqlplugin.Tx) error {
			return m.conflictResolveWorkflowExecutionTx(ctx, tx, request)
		})
}

func (m *sqlExecutionManager) conflictResolveWorkflowExecutionTx(
	ctx context.Context,
	tx sqlplugin.Tx,
	request *p.InternalConflictResolveWorkflowExecutionRequest,
) error {

	currentWorkflow := request.CurrentWorkflowMutation
	resetWorkflow := request.ResetWorkflowSnapshot
	newWorkflow := request.NewWorkflowSnapshot

	shardID := m.shardID

	namespaceID := primitives.MustParseUUID(resetWorkflow.ExecutionInfo.NamespaceId)
	workflowID := resetWorkflow.ExecutionInfo.WorkflowId

	if err := p.ValidateConflictResolveWorkflowModeState(
		request.Mode,
		resetWorkflow,
		newWorkflow,
		currentWorkflow,
	); err != nil {
		return err
	}

	switch request.Mode {
	case p.ConflictResolveWorkflowModeBypassCurrent:
		// do nothing
	case p.ConflictResolveWorkflowModeUpdateCurrent:
		executionState := resetWorkflow.ExecutionState
		lastWriteVersion := resetWorkflow.LastWriteVersion
		startVersion := resetWorkflow.ExecutionInfo.StartVersion
		if newWorkflow != nil {
			executionState = newWorkflow.ExecutionState
			lastWriteVersion = newWorkflow.LastWriteVersion
			startVersion = newWorkflow.ExecutionInfo.StartVersion
		}
		runID := primitives.MustParseUUID(executionState.RunId)
		createRequestID := executionState.CreateRequestId
		state := executionState.State
		status := executionState.Status

		if currentWorkflow != nil {
			prevRunID := primitives.MustParseUUID(currentWorkflow.ExecutionState.RunId)

			if err := assertRunIDAndUpdateCurrentExecution(ctx,
				tx,
				m.shardID,
				namespaceID,
				workflowID,
				runID,
				prevRunID,
				createRequestID,
				state,
				status,
				startVersion,
				lastWriteVersion,
			); err != nil {
				return serviceerror.NewInternal(fmt.Sprintf("ConflictResolveWorkflowExecution. Failed to comare and swap the current record. Error: %v", err))
			}
		} else {
			// reset workflow is current
			prevRunID := primitives.MustParseUUID(resetWorkflow.ExecutionState.RunId)

			if err := assertRunIDAndUpdateCurrentExecution(ctx,
				tx,
				m.shardID,
				namespaceID,
				workflowID,
				runID,
				prevRunID,
				createRequestID,
				state,
				status,
				startVersion,
				lastWriteVersion,
			); err != nil {
				return serviceerror.NewInternal(fmt.Sprintf("ConflictResolveWorkflowExecution. Failed to comare and swap the current record. Error: %v", err))
			}
		}

	default:
		return serviceerror.NewInternal(fmt.Sprintf("ConflictResolveWorkflowExecution: unknown mode: %v", request.Mode))
	}

	if err := applyWorkflowSnapshotTxAsReset(ctx,
		tx,
		shardID,
		&resetWorkflow,
	); err != nil {
		return err
	}

	if currentWorkflow != nil {
		if err := applyWorkflowMutationTx(ctx,
			tx,
			shardID,
			currentWorkflow,
		); err != nil {
			return err
		}
	}

	if newWorkflow != nil {
		if err := m.applyWorkflowSnapshotTxAsNew(ctx,
			tx,
			shardID,
			newWorkflow,
		); err != nil {
			return err
		}
	}
	return nil
}

func (m *sqlExecutionManager) DeleteWorkflowExecution(
	request *p.DeleteWorkflowExecutionRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	namespaceID := primitives.MustParseUUID(request.NamespaceID)
	runID := primitives.MustParseUUID(request.RunID)
	_, err := m.db.DeleteFromExecutions(ctx, sqlplugin.ExecutionsFilter{
		ShardID:     m.shardID,
		NamespaceID: namespaceID,
		WorkflowID:  request.WorkflowID,
		RunID:       runID,
	})
	return err
}

// its possible for a new run of the same workflow to have started after the run we are deleting
// here was finished. In that case, current_executions table will have the same workflowID but different
// runID. The following code will delete the row from current_executions if and only if the runID is
// same as the one we are trying to delete here
func (m *sqlExecutionManager) DeleteCurrentWorkflowExecution(
	request *p.DeleteCurrentWorkflowExecutionRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	namespaceID := primitives.MustParseUUID(request.NamespaceID)
	runID := primitives.MustParseUUID(request.RunID)
	_, err := m.db.DeleteFromCurrentExecutions(ctx, sqlplugin.CurrentExecutionsFilter{
		ShardID:     m.shardID,
		NamespaceID: namespaceID,
		WorkflowID:  request.WorkflowID,
		RunID:       runID,
	})
	return err
}

func (m *sqlExecutionManager) GetCurrentExecution(
	request *p.GetCurrentExecutionRequest,
) (*p.GetCurrentExecutionResponse, error) {
	ctx, cancel := newExecutionContext()
	defer cancel()
	row, err := m.db.SelectFromCurrentExecutions(ctx, sqlplugin.CurrentExecutionsFilter{
		ShardID:     m.shardID,
		NamespaceID: primitives.MustParseUUID(request.NamespaceID),
		WorkflowID:  request.WorkflowID,
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, serviceerror.NewNotFound(err.Error())
		}
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetCurrentExecution operation failed. Error: %v", err))
	}
	return &p.GetCurrentExecutionResponse{
		StartRequestID:   row.CreateRequestID,
		RunID:            row.RunID.String(),
		State:            row.State,
		Status:           row.Status,
		LastWriteVersion: row.LastWriteVersion,
	}, nil
}

func (m *sqlExecutionManager) ListConcreteExecutions(
	_ *p.ListConcreteExecutionsRequest,
) (*p.InternalListConcreteExecutionsResponse, error) {
	return nil, serviceerror.NewUnimplemented("ListConcreteExecutions is not implemented")
}

func (m *sqlExecutionManager) GetTransferTask(
	request *persistence.GetTransferTaskRequest,
) (*persistence.GetTransferTaskResponse, error) {
	ctx, cancel := newExecutionContext()
	defer cancel()
	rows, err := m.db.SelectFromTransferTasks(ctx, sqlplugin.TransferTasksFilter{
		ShardID: request.ShardID,
		TaskID:  request.TaskID,
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, serviceerror.NewNotFound(fmt.Sprintf("GetTransferTask operation failed. Task with ID %v not found. Error: %v", request.TaskID, err))
		}
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetTransferTask operation failed. Failed to get record. TaskId: %v. Error: %v", request.TaskID, err))
	}

	if len(rows) == 0 {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetTransferTask operation failed. Failed to get record. TaskId: %v", request.TaskID))
	}

	transferRow := rows[0]
	transferInfo, err := serialization.TransferTaskInfoFromBlob(transferRow.Data, transferRow.DataEncoding)
	if err != nil {
		return nil, err
	}

	resp := &persistence.GetTransferTaskResponse{TransferTaskInfo: transferInfo}

	return resp, nil
}

func (m *sqlExecutionManager) GetTransferTasks(
	request *p.GetTransferTasksRequest,
) (*p.GetTransferTasksResponse, error) {
	ctx, cancel := newExecutionContext()
	defer cancel()
	rows, err := m.db.RangeSelectFromTransferTasks(ctx, sqlplugin.TransferTasksRangeFilter{
		ShardID:   m.shardID,
		MinTaskID: request.ReadLevel,
		MaxTaskID: request.MaxReadLevel,
	})
	if err != nil {
		if err != sql.ErrNoRows {
			return nil, serviceerror.NewInternal(fmt.Sprintf("GetTransferTasks operation failed. Select failed. Error: %v", err))
		}
	}
	resp := &p.GetTransferTasksResponse{Tasks: make([]*persistencespb.TransferTaskInfo, len(rows))}
	for i, row := range rows {
		info, err := serialization.TransferTaskInfoFromBlob(row.Data, row.DataEncoding)
		if err != nil {
			return nil, err
		}
		resp.Tasks[i] = info
	}

	return resp, nil
}

func (m *sqlExecutionManager) CompleteTransferTask(
	request *p.CompleteTransferTaskRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	if _, err := m.db.DeleteFromTransferTasks(ctx, sqlplugin.TransferTasksFilter{
		ShardID: m.shardID,
		TaskID:  request.TaskID,
	}); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("CompleteTransferTask operation failed. Error: %v", err))
	}
	return nil
}

func (m *sqlExecutionManager) RangeCompleteTransferTask(
	request *p.RangeCompleteTransferTaskRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	if _, err := m.db.RangeDeleteFromTransferTasks(ctx, sqlplugin.TransferTasksRangeFilter{
		ShardID:   m.shardID,
		MinTaskID: request.ExclusiveBeginTaskID,
		MaxTaskID: request.InclusiveEndTaskID,
	}); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("RangeCompleteTransferTask operation failed. Error: %v", err))
	}
	return nil
}

func (m *sqlExecutionManager) GetReplicationTask(
	request *persistence.GetReplicationTaskRequest,
) (*persistence.GetReplicationTaskResponse, error) {
	ctx, cancel := newExecutionContext()
	defer cancel()
	rows, err := m.db.SelectFromReplicationTasks(ctx, sqlplugin.ReplicationTasksFilter{
		ShardID: request.ShardID,
		TaskID:  request.TaskID,
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, serviceerror.NewNotFound(fmt.Sprintf("GetReplicationTask operation failed. Task with ID %v not found. Error: %v", request.TaskID, err))
		}
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetReplicationTask operation failed. Failed to get record. TaskId: %v. Error: %v", request.TaskID, err))
	}

	if len(rows) == 0 {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetReplicationTask operation failed. Failed to get record. TaskId: %v", request.TaskID))
	}

	replicationRow := rows[0]
	replicationInfo, err := serialization.ReplicationTaskInfoFromBlob(replicationRow.Data, replicationRow.DataEncoding)
	if err != nil {
		return nil, err
	}

	resp := &persistence.GetReplicationTaskResponse{ReplicationTaskInfo: replicationInfo}

	return resp, nil
}

func (m *sqlExecutionManager) GetReplicationTasks(
	request *p.GetReplicationTasksRequest,
) (*p.GetReplicationTasksResponse, error) {
	ctx, cancel := newExecutionContext()
	defer cancel()
	readLevel, maxReadLevelInclusive, err := getReadLevels(request)
	if err != nil {
		return nil, err
	}

	rows, err := m.db.RangeSelectFromReplicationTasks(ctx, sqlplugin.ReplicationTasksRangeFilter{
		ShardID:   m.shardID,
		MinTaskID: readLevel,
		MaxTaskID: maxReadLevelInclusive,
		PageSize:  request.BatchSize,
	})

	switch err {
	case nil:
		return m.populateGetReplicationTasksResponse(rows, request.MaxReadLevel)
	case sql.ErrNoRows:
		return &p.GetReplicationTasksResponse{}, nil
	default:
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetReplicationTasks operation failed. Select failed: %v", err))
	}
}

func getReadLevels(
	request *p.GetReplicationTasksRequest,
) (readLevel int64, maxReadLevelInclusive int64, err error) {
	readLevel = request.ReadLevel
	if len(request.NextPageToken) > 0 {
		readLevel, err = deserializePageToken(request.NextPageToken)
		if err != nil {
			return 0, 0, err
		}
	}

	maxReadLevelInclusive = collection.MaxInt64(readLevel+int64(request.BatchSize), request.MaxReadLevel)
	return readLevel, maxReadLevelInclusive, nil
}

func (m *sqlExecutionManager) populateGetReplicationTasksResponse(
	rows []sqlplugin.ReplicationTasksRow,
	requestMaxReadLevel int64,
) (*p.GetReplicationTasksResponse, error) {
	if len(rows) == 0 {
		return &p.GetReplicationTasksResponse{}, nil
	}

	var tasks = make([]*persistencespb.ReplicationTaskInfo, len(rows))
	for i, row := range rows {
		info, err := serialization.ReplicationTaskInfoFromBlob(row.Data, row.DataEncoding)
		if err != nil {
			return nil, err
		}

		tasks[i] = info
	}
	var nextPageToken []byte
	lastTaskID := rows[len(rows)-1].TaskID
	if lastTaskID < requestMaxReadLevel {
		nextPageToken = serializePageToken(lastTaskID)
	}
	return &p.GetReplicationTasksResponse{
		Tasks:         tasks,
		NextPageToken: nextPageToken,
	}, nil
}

func (m *sqlExecutionManager) populateGetReplicationDLQTasksResponse(
	rows []sqlplugin.ReplicationDLQTasksRow,
	requestMaxReadLevel int64,
) (*p.GetReplicationTasksResponse, error) {
	if len(rows) == 0 {
		return &p.GetReplicationTasksResponse{}, nil
	}

	var tasks = make([]*persistencespb.ReplicationTaskInfo, len(rows))
	for i, row := range rows {
		info, err := serialization.ReplicationTaskInfoFromBlob(row.Data, row.DataEncoding)
		if err != nil {
			return nil, err
		}

		tasks[i] = info
	}
	var nextPageToken []byte
	lastTaskID := rows[len(rows)-1].TaskID
	if lastTaskID < requestMaxReadLevel {
		nextPageToken = serializePageToken(lastTaskID)
	}
	return &p.GetReplicationTasksResponse{
		Tasks:         tasks,
		NextPageToken: nextPageToken,
	}, nil
}

func (m *sqlExecutionManager) CompleteReplicationTask(
	request *p.CompleteReplicationTaskRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	if _, err := m.db.DeleteFromReplicationTasks(ctx, sqlplugin.ReplicationTasksFilter{
		ShardID: m.shardID,
		TaskID:  request.TaskID,
	}); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("CompleteReplicationTask operation failed. Error: %v", err))
	}
	return nil
}

func (m *sqlExecutionManager) RangeCompleteReplicationTask(
	request *p.RangeCompleteReplicationTaskRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	if _, err := m.db.RangeDeleteFromReplicationTasks(ctx, sqlplugin.ReplicationTasksRangeFilter{
		ShardID:   m.shardID,
		MinTaskID: 0,
		MaxTaskID: request.InclusiveEndTaskID,
	}); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("RangeCompleteReplicationTask operation failed. Error: %v", err))
	}
	return nil
}

func (m *sqlExecutionManager) GetReplicationTasksFromDLQ(
	request *p.GetReplicationTasksFromDLQRequest,
) (*p.GetReplicationTasksFromDLQResponse, error) {
	ctx, cancel := newExecutionContext()
	defer cancel()
	readLevel, maxReadLevelInclusive, err := getReadLevels(&request.GetReplicationTasksRequest)
	if err != nil {
		return nil, err
	}

	rows, err := m.db.RangeSelectFromReplicationDLQTasks(ctx, sqlplugin.ReplicationDLQTasksRangeFilter{
		ShardID:           m.shardID,
		MinTaskID:         readLevel,
		MaxTaskID:         maxReadLevelInclusive,
		PageSize:          request.BatchSize,
		SourceClusterName: request.SourceClusterName,
	})

	switch err {
	case nil:
		return m.populateGetReplicationDLQTasksResponse(rows, request.MaxReadLevel)
	case sql.ErrNoRows:
		return &p.GetReplicationTasksResponse{}, nil
	default:
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetReplicationTasks operation failed. Select failed: %v", err))
	}
}

func (m *sqlExecutionManager) DeleteReplicationTaskFromDLQ(
	request *p.DeleteReplicationTaskFromDLQRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	if _, err := m.db.DeleteFromReplicationDLQTasks(ctx, sqlplugin.ReplicationDLQTasksFilter{
		ShardID:           m.shardID,
		TaskID:            request.TaskID,
		SourceClusterName: request.SourceClusterName,
	}); err != nil {
		return err
	}
	return nil
}

func (m *sqlExecutionManager) RangeDeleteReplicationTaskFromDLQ(
	request *p.RangeDeleteReplicationTaskFromDLQRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	if _, err := m.db.RangeDeleteFromReplicationDLQTasks(ctx, sqlplugin.ReplicationDLQTasksRangeFilter{
		ShardID:           m.shardID,
		SourceClusterName: request.SourceClusterName,
		MinTaskID:         request.ExclusiveBeginTaskID,
		MaxTaskID:         request.InclusiveEndTaskID,
	}); err != nil {
		return err
	}
	return nil
}

func (m *sqlExecutionManager) GetTimerTask(
	request *persistence.GetTimerTaskRequest,
) (*persistence.GetTimerTaskResponse, error) {
	ctx, cancel := newExecutionContext()
	defer cancel()
	rows, err := m.db.SelectFromTimerTasks(ctx, sqlplugin.TimerTasksFilter{
		ShardID:             request.ShardID,
		TaskID:              request.TaskID,
		VisibilityTimestamp: request.VisibilityTimestamp,
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, serviceerror.NewNotFound(fmt.Sprintf("GetTimerTask operation failed. Task with ID %v not found. Error: %v", request.TaskID, err))
		}
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetTimerTask operation failed. Failed to get record. TaskId: %v. Error: %v", request.TaskID, err))
	}

	if len(rows) == 0 {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetTimerTask operation failed. Failed to get record. TaskId: %v", request.TaskID))
	}

	timerRow := rows[0]
	timerInfo, err := serialization.TimerTaskInfoFromBlob(timerRow.Data, timerRow.DataEncoding)
	if err != nil {
		return nil, err
	}

	resp := &persistence.GetTimerTaskResponse{TimerTaskInfo: timerInfo}

	return resp, nil
}

func (m *sqlExecutionManager) GetTimerIndexTasks(
	request *p.GetTimerIndexTasksRequest,
) (*p.GetTimerIndexTasksResponse, error) {
	ctx, cancel := newExecutionContext()
	defer cancel()
	pageToken := &timerTaskPageToken{TaskID: math.MinInt64, Timestamp: request.MinTimestamp}
	if len(request.NextPageToken) > 0 {
		if err := pageToken.deserialize(request.NextPageToken); err != nil {
			return nil, serviceerror.NewInternal(fmt.Sprintf("error deserializing timerTaskPageToken: %v", err))
		}
	}

	rows, err := m.db.RangeSelectFromTimerTasks(ctx, sqlplugin.TimerTasksRangeFilter{
		ShardID:                m.shardID,
		MinVisibilityTimestamp: pageToken.Timestamp,
		TaskID:                 pageToken.TaskID,
		MaxVisibilityTimestamp: request.MaxTimestamp,
		PageSize:               request.BatchSize + 1,
	})

	if err != nil && err != sql.ErrNoRows {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetTimerTasks operation failed. Select failed. Error: %v", err))
	}

	resp := &p.GetTimerIndexTasksResponse{Timers: make([]*persistencespb.TimerTaskInfo, len(rows))}
	for i, row := range rows {
		info, err := serialization.TimerTaskInfoFromBlob(row.Data, row.DataEncoding)
		if err != nil {
			return nil, err
		}
		resp.Timers[i] = info
	}

	if len(resp.Timers) > request.BatchSize {
		goVisibilityTimestamp := resp.Timers[request.BatchSize].VisibilityTime
		if goVisibilityTimestamp == nil {
			return nil, serviceerror.NewInternal(fmt.Sprintf("GetTimerTasks: time for page token is nil - TaskId '%v'", resp.Timers[request.BatchSize].TaskId))
		}

		pageToken = &timerTaskPageToken{
			TaskID:    resp.Timers[request.BatchSize].GetTaskId(),
			Timestamp: *goVisibilityTimestamp,
		}
		resp.Timers = resp.Timers[:request.BatchSize]
		nextToken, err := pageToken.serialize()
		if err != nil {
			return nil, serviceerror.NewInternal(fmt.Sprintf("GetTimerTasks: error serializing page token: %v", err))
		}
		resp.NextPageToken = nextToken
	}

	return resp, nil
}

func (m *sqlExecutionManager) CompleteTimerTask(
	request *p.CompleteTimerTaskRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	if _, err := m.db.DeleteFromTimerTasks(ctx, sqlplugin.TimerTasksFilter{
		ShardID:             m.shardID,
		VisibilityTimestamp: request.VisibilityTimestamp,
		TaskID:              request.TaskID,
	}); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("CompleteTimerTask operation failed. Error: %v", err))
	}
	return nil
}

func (m *sqlExecutionManager) RangeCompleteTimerTask(
	request *p.RangeCompleteTimerTaskRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	start := request.InclusiveBeginTimestamp
	end := request.ExclusiveEndTimestamp
	if _, err := m.db.RangeDeleteFromTimerTasks(ctx, sqlplugin.TimerTasksRangeFilter{
		ShardID:                m.shardID,
		MinVisibilityTimestamp: start,
		MaxVisibilityTimestamp: end,
	}); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("CompleteTimerTask operation failed. Error: %v", err))
	}
	return nil
}

func (m *sqlExecutionManager) GetVisibilityTask(
	request *persistence.GetVisibilityTaskRequest,
) (*persistence.GetVisibilityTaskResponse, error) {
	ctx, cancel := newExecutionContext()
	defer cancel()
	rows, err := m.db.SelectFromVisibilityTasks(ctx, sqlplugin.VisibilityTasksFilter{
		ShardID: request.ShardID,
		TaskID:  request.TaskID,
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, serviceerror.NewNotFound(fmt.Sprintf("GetVisibilityTask operation failed. Task with ID %v not found. Error: %v", request.TaskID, err))
		}
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetVisibilityTask operation failed. Failed to get record. TaskId: %v. Error: %v", request.TaskID, err))
	}

	if len(rows) == 0 {
		return nil, serviceerror.NewInternal(fmt.Sprintf("GetVisibilityTask operation failed. Failed to get record. TaskId: %v", request.TaskID))
	}

	visibilityRow := rows[0]
	visibilityInfo, err := serialization.VisibilityTaskInfoFromBlob(visibilityRow.Data, visibilityRow.DataEncoding)
	if err != nil {
		return nil, err
	}

	resp := &persistence.GetVisibilityTaskResponse{VisibilityTaskInfo: visibilityInfo}

	return resp, nil
}

func (m *sqlExecutionManager) GetVisibilityTasks(
	request *p.GetVisibilityTasksRequest,
) (*p.GetVisibilityTasksResponse, error) {
	ctx, cancel := newExecutionContext()
	defer cancel()
	rows, err := m.db.RangeSelectFromVisibilityTasks(ctx, sqlplugin.VisibilityTasksRangeFilter{
		ShardID:   m.shardID,
		MinTaskID: request.ReadLevel,
		MaxTaskID: request.MaxReadLevel,
	})
	if err != nil {
		if err != sql.ErrNoRows {
			return nil, serviceerror.NewInternal(fmt.Sprintf("GetVisibilityTasks operation failed. Select failed. Error: %v", err))
		}
	}
	resp := &p.GetVisibilityTasksResponse{Tasks: make([]*persistencespb.VisibilityTaskInfo, len(rows))}
	for i, row := range rows {
		info, err := serialization.VisibilityTaskInfoFromBlob(row.Data, row.DataEncoding)
		if err != nil {
			return nil, err
		}
		resp.Tasks[i] = info
	}

	return resp, nil
}

func (m *sqlExecutionManager) CompleteVisibilityTask(
	request *p.CompleteVisibilityTaskRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	if _, err := m.db.DeleteFromVisibilityTasks(ctx, sqlplugin.VisibilityTasksFilter{
		ShardID: m.shardID,
		TaskID:  request.TaskID,
	}); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("CompleteVisibilityTask operation failed. Error: %v", err))
	}
	return nil
}

func (m *sqlExecutionManager) RangeCompleteVisibilityTask(
	request *p.RangeCompleteVisibilityTaskRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	if _, err := m.db.RangeDeleteFromVisibilityTasks(ctx, sqlplugin.VisibilityTasksRangeFilter{
		ShardID:   m.shardID,
		MinTaskID: request.ExclusiveBeginTaskID,
		MaxTaskID: request.InclusiveEndTaskID,
	}); err != nil {
		return serviceerror.NewInternal(fmt.Sprintf("RangeCompleteVisibilityTask operation failed. Error: %v", err))
	}
	return nil
}

func (m *sqlExecutionManager) PutReplicationTaskToDLQ(
	request *p.PutReplicationTaskToDLQRequest,
) error {
	ctx, cancel := newExecutionContext()
	defer cancel()
	replicationTask := request.TaskInfo
	blob, err := serialization.ReplicationTaskInfoToBlob(replicationTask)

	if err != nil {
		return err
	}

	_, err = m.db.InsertIntoReplicationDLQTasks(ctx, []sqlplugin.ReplicationDLQTasksRow{{
		SourceClusterName: request.SourceClusterName,
		ShardID:           m.shardID,
		TaskID:            replicationTask.GetTaskId(),
		Data:              blob.Data,
		DataEncoding:      blob.EncodingType.String(),
	}})

	// Tasks are immutable. So it's fine if we already persisted it before.
	// This can happen when tasks are retried (ack and cleanup can have lag on source side).
	if err != nil && !m.db.IsDupEntryError(err) {
		return serviceerror.NewInternal(fmt.Sprintf("Failed to create replication tasks. Error: %v", err))
	}

	return nil
}

type timerTaskPageToken struct {
	TaskID    int64
	Timestamp time.Time
}

func (t *timerTaskPageToken) serialize() ([]byte, error) {
	return json.Marshal(t)
}

func (t *timerTaskPageToken) deserialize(payload []byte) error {
	return json.Unmarshal(payload, t)
}
