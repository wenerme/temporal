// Copyright (c) 2019 Uber Technologies, Inc.
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

//go:generate mockgen -copyright_file ../../LICENSE -package $GOPACKAGE -source $GOFILE -destination nDCHistoryResender_mock.go

package xdc

import (
	"context"
	"time"

	commonpb "go.temporal.io/temporal-proto/common"
	executionpb "go.temporal.io/temporal-proto/execution"

	"github.com/temporalio/temporal/.gen/proto/adminservice"
	eventgenpb "github.com/temporalio/temporal/.gen/proto/event"
	"github.com/temporalio/temporal/.gen/proto/historyservice"
	"github.com/temporalio/temporal/client/admin"
	"github.com/temporalio/temporal/common/cache"
	"github.com/temporalio/temporal/common/collection"
	"github.com/temporalio/temporal/common/log"
	"github.com/temporalio/temporal/common/log/tag"
	"github.com/temporalio/temporal/common/persistence"
	"github.com/temporalio/temporal/common/rpc"
)

const (
	resendContextTimeout = 30 * time.Second
)

type (
	// nDCHistoryReplicationFn provides the functionality to deliver replication raw history request to history
	// the provided func should be thread safe
	nDCHistoryReplicationFn func(ctx context.Context, request *historyservice.ReplicateEventsV2Request) error

	// NDCHistoryResender is the interface for resending history events to remote
	NDCHistoryResender interface {
		// SendSingleWorkflowHistory sends multiple run IDs's history events to remote
		SendSingleWorkflowHistory(
			namespaceID string,
			workflowID string,
			runID string,
			startEventID int64,
			startEventVersion int64,
			endEventID int64,
			endEventVersion int64,
		) error
	}

	// NDCHistoryResenderImpl is the implementation of NDCHistoryResender
	NDCHistoryResenderImpl struct {
		namespaceCache       cache.NamespaceCache
		adminClient          admin.Client
		historyReplicationFn nDCHistoryReplicationFn
		serializer           persistence.PayloadSerializer
		logger               log.Logger
	}

	historyBatch struct {
		versionHistory *eventgenpb.VersionHistory
		rawEventBatch  *commonpb.DataBlob
	}
)

// NewNDCHistoryResender create a new NDCHistoryResenderImpl
func NewNDCHistoryResender(
	namespaceCache cache.NamespaceCache,
	adminClient admin.Client,
	historyReplicationFn nDCHistoryReplicationFn,
	serializer persistence.PayloadSerializer,
	logger log.Logger,
) *NDCHistoryResenderImpl {

	return &NDCHistoryResenderImpl{
		namespaceCache:       namespaceCache,
		adminClient:          adminClient,
		historyReplicationFn: historyReplicationFn,
		serializer:           serializer,
		logger:               logger,
	}
}

// SendSingleWorkflowHistory sends one run IDs's history events to remote
func (n *NDCHistoryResenderImpl) SendSingleWorkflowHistory(
	namespaceID string,
	workflowID string,
	runID string,
	startEventID int64,
	startEventVersion int64,
	endEventID int64,
	endEventVersion int64,
) error {

	historyIterator := collection.NewPagingIterator(n.getPaginationFn(
		namespaceID,
		workflowID,
		runID,
		startEventID,
		startEventVersion,
		endEventID,
		endEventVersion))

	for historyIterator.HasNext() {
		result, err := historyIterator.Next()
		if err != nil {
			n.logger.Error("failed to get history events",
				tag.WorkflowNamespaceID(namespaceID),
				tag.WorkflowID(workflowID),
				tag.WorkflowRunID(runID),
				tag.Error(err))
			return err
		}
		historyBatch := result.(*historyBatch)

		replicationRequest := n.createReplicationRawRequest(
			namespaceID,
			workflowID,
			runID,
			historyBatch.rawEventBatch,
			historyBatch.versionHistory.GetItems())

		err = n.sendReplicationRawRequest(replicationRequest)
		if err != nil {
			n.logger.Error("failed to replicate events",
				tag.WorkflowNamespaceID(namespaceID),
				tag.WorkflowID(workflowID),
				tag.WorkflowRunID(runID),
				tag.Error(err))
			return err
		}
	}
	return nil
}

func (n *NDCHistoryResenderImpl) getPaginationFn(
	namespaceID string,
	workflowID string,
	runID string,
	startEventID int64,
	startEventVersion int64,
	endEventID int64,
	endEventVersion int64,
) collection.PaginationFn {

	return func(paginationToken []byte) ([]interface{}, []byte, error) {

		response, err := n.getHistory(
			namespaceID,
			workflowID,
			runID,
			startEventID,
			startEventVersion,
			endEventID,
			endEventVersion,
			paginationToken,
			defaultPageSize,
		)
		if err != nil {
			return nil, nil, err
		}

		var paginateItems []interface{}
		versionHistory := response.GetVersionHistory()
		for _, history := range response.GetHistoryBatches() {
			batch := &historyBatch{
				versionHistory: versionHistory,
				rawEventBatch:  history,
			}
			paginateItems = append(paginateItems, batch)
		}
		return paginateItems, response.NextPageToken, nil
	}
}

func (n *NDCHistoryResenderImpl) createReplicationRawRequest(
	namespaceID string,
	workflowID string,
	runID string,
	historyBlob *commonpb.DataBlob,
	versionHistoryItems []*eventgenpb.VersionHistoryItem,
) *historyservice.ReplicateEventsV2Request {

	request := &historyservice.ReplicateEventsV2Request{
		NamespaceId: namespaceID,
		WorkflowExecution: &executionpb.WorkflowExecution{
			WorkflowId: workflowID,
			RunId:      runID,
		},
		Events:              historyBlob,
		VersionHistoryItems: versionHistoryItems,
	}
	return request
}

func (n *NDCHistoryResenderImpl) sendReplicationRawRequest(
	request *historyservice.ReplicateEventsV2Request,
) error {

	ctx, cancel := context.WithTimeout(context.Background(), resendContextTimeout)
	defer cancel()
	return n.historyReplicationFn(ctx, request)
}

func (n *NDCHistoryResenderImpl) getHistory(
	namespaceID string,
	workflowID string,
	runID string,
	startEventID int64,
	startEventVersion int64,
	endEventID int64,
	endEventVersion int64,
	token []byte,
	pageSize int32,
) (*adminservice.GetWorkflowExecutionRawHistoryV2Response, error) {

	logger := n.logger.WithTags(tag.WorkflowRunID(runID))

	namespaceEntry, err := n.namespaceCache.GetNamespaceByID(namespaceID)
	if err != nil {
		logger.Error("error getting namespace", tag.Error(err))
		return nil, err
	}
	namespace := namespaceEntry.GetInfo().Name

	ctx, cancel := rpc.NewContextWithTimeoutAndHeaders(resendContextTimeout)
	defer cancel()
	response, err := n.adminClient.GetWorkflowExecutionRawHistoryV2(ctx, &adminservice.GetWorkflowExecutionRawHistoryV2Request{
		Namespace: namespace,
		Execution: &executionpb.WorkflowExecution{
			WorkflowId: workflowID,
			RunId:      runID,
		},
		StartEventId:      startEventID,
		StartEventVersion: startEventVersion,
		EndEventId:        endEventID,
		EndEventVersion:   endEventVersion,
		MaximumPageSize:   pageSize,
		NextPageToken:     token,
	})
	if err != nil {
		logger.Error("error getting history", tag.Error(err))
		return nil, err
	}

	return response, nil
}
