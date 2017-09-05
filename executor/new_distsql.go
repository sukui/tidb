// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package executor

import (
	"sort"
	"sync"
	"sync/atomic"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/tidb/context"
	"github.com/pingcap/tidb/distsql"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/types"
	"github.com/pingcap/tipb/go-tipb"
	goctx "golang.org/x/net/context"
)

var (
	_ Executor = &TableReaderExecutor{}
	_ Executor = &IndexReaderExecutor{}
	_ Executor = &IndexLookUpExecutor{}
)

// DataReader can send requests which ranges are constructed by datums.
type DataReader interface {
	Executor

	doRequestForDatums(datums [][]types.Datum, goCtx goctx.Context) error
}

// handleIsExtra checks whether this column is a extra handle column generated during plan building phase.
func handleIsExtra(col *expression.Column) bool {
	if col != nil && col.ID == model.ExtraHandleID {
		return true
	}
	return false
}

// TableReaderExecutor sends dag request and reads table data from kv layer.
type TableReaderExecutor struct {
	table     table.Table
	tableID   int64
	keepOrder bool
	desc      bool
	ranges    []types.IntColumnRange
	dagPB     *tipb.DAGRequest
	ctx       context.Context
	schema    *expression.Schema
	// columns are only required by union scan.
	columns []*model.ColumnInfo
	// This is the column that represent the handle, we can use handleCol.Index to know its position.
	handleCol *expression.Column

	// result returns one or more distsql.PartialResult and each PartialResult is returned by one region.
	result        distsql.SelectResult
	partialResult distsql.PartialResult
	priority      int
}

// Schema implements the Executor Schema interface.
func (e *TableReaderExecutor) Schema() *expression.Schema {
	return e.schema
}

// Close implements the Executor Close interface.
func (e *TableReaderExecutor) Close() error {
	err := closeAll(e.result, e.partialResult)
	e.result = nil
	e.partialResult = nil
	return errors.Trace(err)
}

// Next implements the Executor Next interface.
func (e *TableReaderExecutor) Next() (Row, error) {
	for {
		// Get partial result.
		if e.partialResult == nil {
			var err error
			e.partialResult, err = e.result.Next()
			if err != nil {
				return nil, errors.Trace(err)
			}
			if e.partialResult == nil {
				// Finished.
				return nil, nil
			}
		}
		// Get a row from partial result.
		h, rowData, err := e.partialResult.Next()
		if err != nil {
			return nil, errors.Trace(err)
		}
		if rowData == nil {
			// Finish the current partial result and get the next one.
			e.partialResult.Close()
			e.partialResult = nil
			continue
		}
		values := make([]types.Datum, e.schema.Len())
		if handleIsExtra(e.handleCol) {
			err = codec.SetRawValues(rowData, values[:len(values)-1])
			values[len(values)-1].SetInt64(h)
		} else {
			err = codec.SetRawValues(rowData, values)
		}
		if err != nil {
			return nil, errors.Trace(err)
		}
		err = decodeRawValues(values, e.schema, e.ctx.GetSessionVars().GetTimeZone())
		if err != nil {
			return nil, errors.Trace(err)
		}
		return values, nil
	}
}

// Open implements the Executor Open interface.
func (e *TableReaderExecutor) Open() error {
	kvRanges := tableRangesToKVRanges(e.tableID, e.ranges)
	var err error
	e.result, err = distsql.SelectDAG(e.ctx.GetClient(), goctx.Background(), e.dagPB, kvRanges, e.ctx.GetSessionVars().DistSQLScanConcurrency, e.keepOrder, e.desc, getIsolationLevel(e.ctx.GetSessionVars()), e.priority)
	if err != nil {
		return errors.Trace(err)
	}
	e.result.Fetch(e.ctx.GoCtx())
	return nil
}

// doRequestForHandles constructs kv ranges by handles. It is used by index look up executor.
func (e *TableReaderExecutor) doRequestForHandles(handles []int64, goCtx goctx.Context) error {
	sort.Sort(int64Slice(handles))
	kvRanges := tableHandlesToKVRanges(e.tableID, handles)
	var err error
	e.result, err = distsql.SelectDAG(e.ctx.GetClient(), goCtx, e.dagPB, kvRanges, e.ctx.GetSessionVars().DistSQLScanConcurrency, e.keepOrder, e.desc, getIsolationLevel(e.ctx.GetSessionVars()), e.priority)
	if err != nil {
		return errors.Trace(err)
	}
	e.result.Fetch(goCtx)
	return nil
}

// doRequestForDatums constructs kv ranges by Datums. It is used by index look up join.
// Every lens for `datums` will always be one and must be type of int64.
func (e *TableReaderExecutor) doRequestForDatums(datums [][]types.Datum, goCtx goctx.Context) error {
	handles := make([]int64, 0, len(datums))
	for _, datum := range datums {
		handles = append(handles, datum[0].GetInt64())
	}
	return errors.Trace(e.doRequestForHandles(handles, goCtx))
}

// IndexReaderExecutor sends dag request and reads index data from kv layer.
type IndexReaderExecutor struct {
	table     table.Table
	index     *model.IndexInfo
	tableID   int64
	keepOrder bool
	desc      bool
	ranges    []*types.IndexRange
	dagPB     *tipb.DAGRequest
	ctx       context.Context
	schema    *expression.Schema
	// This is the column that represent the handle, we can use handleCol.Index to know its position.
	handleCol *expression.Column

	// result returns one or more distsql.PartialResult and each PartialResult is returned by one region.
	result        distsql.SelectResult
	partialResult distsql.PartialResult
	// columns are only required by union scan.
	columns  []*model.ColumnInfo
	priority int
}

// Schema implements the Executor Schema interface.
func (e *IndexReaderExecutor) Schema() *expression.Schema {
	return e.schema
}

// Close implements the Executor Close interface.
func (e *IndexReaderExecutor) Close() error {
	err := closeAll(e.result, e.partialResult)
	e.result = nil
	e.partialResult = nil
	return errors.Trace(err)
}

// Next implements the Executor Next interface.
func (e *IndexReaderExecutor) Next() (Row, error) {
	for {
		// Get partial result.
		if e.partialResult == nil {
			var err error
			e.partialResult, err = e.result.Next()
			if err != nil {
				return nil, errors.Trace(err)
			}
			if e.partialResult == nil {
				// Finished.
				return nil, nil
			}
		}
		// Get a row from partial result.
		h, rowData, err := e.partialResult.Next()
		if err != nil {
			return nil, errors.Trace(err)
		}
		if rowData == nil {
			// Finish the current partial result and get the next one.
			e.partialResult.Close()
			e.partialResult = nil
			continue
		}
		values := make([]types.Datum, e.schema.Len())
		if handleIsExtra(e.handleCol) {
			err = codec.SetRawValues(rowData, values[:len(values)-1])
			values[len(values)-1].SetInt64(h)
		} else {
			err = codec.SetRawValues(rowData, values)
		}
		if err != nil {
			return nil, errors.Trace(err)
		}
		err = decodeRawValues(values, e.schema, e.ctx.GetSessionVars().GetTimeZone())
		if err != nil {
			return nil, errors.Trace(err)
		}
		return values, nil
	}
}

// Open implements the Executor Open interface.
func (e *IndexReaderExecutor) Open() error {
	fieldTypes := make([]*types.FieldType, len(e.index.Columns))
	for i, v := range e.index.Columns {
		fieldTypes[i] = &(e.table.Cols()[v.Offset].FieldType)
	}
	kvRanges, err := indexRangesToKVRanges(e.ctx.GetSessionVars().StmtCtx, e.tableID, e.index.ID, e.ranges, fieldTypes)
	if err != nil {
		return errors.Trace(err)
	}
	e.result, err = distsql.SelectDAG(e.ctx.GetClient(), e.ctx.GoCtx(), e.dagPB, kvRanges, e.ctx.GetSessionVars().DistSQLScanConcurrency, e.keepOrder, e.desc, getIsolationLevel(e.ctx.GetSessionVars()), e.priority)
	if err != nil {
		return errors.Trace(err)
	}
	e.result.Fetch(e.ctx.GoCtx())
	return nil
}

// doRequestForDatums constructs kv ranges by datums. It is used by index look up executor.
func (e *IndexReaderExecutor) doRequestForDatums(values [][]types.Datum, goCtx goctx.Context) error {
	kvRanges, err := indexValuesToKVRanges(e.tableID, e.index.ID, values)
	if err != nil {
		return errors.Trace(err)
	}
	e.result, err = distsql.SelectDAG(e.ctx.GetClient(), e.ctx.GoCtx(), e.dagPB, kvRanges, e.ctx.GetSessionVars().DistSQLScanConcurrency, e.keepOrder, e.desc, getIsolationLevel(e.ctx.GetSessionVars()), e.priority)
	if err != nil {
		return errors.Trace(err)
	}
	e.result.Fetch(goCtx)
	return nil
}

// IndexLookUpExecutor implements double read for index scan.
type IndexLookUpExecutor struct {
	table     table.Table
	index     *model.IndexInfo
	tableID   int64
	keepOrder bool
	desc      bool
	ranges    []*types.IndexRange
	dagPB     *tipb.DAGRequest
	ctx       context.Context
	schema    *expression.Schema
	// This is the column that represent the handle, we can use handleCol.Index to know its position.
	handleCol    *expression.Column
	tableRequest *tipb.DAGRequest
	// columns are only required by union scan.
	columns  []*model.ColumnInfo
	priority int
	// All fields above is immutable.

	indexHandler
	tableHandler
	finished chan struct{}
}

// indexHandler is used by IndexLookUpExecutor to maintain index lookup background goroutines.
type indexHandler struct {
	buildTableTasks func(handles []int64) []*lookupTableTask
	wg              sync.WaitGroup
}

// open the indexHandler, launch some background worker goroutines and write the results to workCh.
func (ih *indexHandler) open(kvRanges []kv.KeyRange, e *IndexLookUpExecutor, workCh chan<- *lookupTableTask, finished <-chan struct{}) error {
	result, err := distsql.SelectDAG(e.ctx.GetClient(), e.ctx.GoCtx(), e.dagPB, kvRanges,
		e.ctx.GetSessionVars().DistSQLScanConcurrency, e.keepOrder, e.desc, getIsolationLevel(e.ctx.GetSessionVars()), e.priority)
	if err != nil {
		return errors.Trace(err)
	}
	result.Fetch(e.ctx.GoCtx())
	ih.buildTableTasks = e.buildTableTasks
	ih.wg.Add(1)
	go func() {
		ctx, cancel := goctx.WithCancel(e.ctx.GoCtx())
		ih.fetchHandles(result, workCh, ctx, finished)
		cancel()
		if err := result.Close(); err != nil {
			log.Error("close SelectDAG result failed:", errors.ErrorStack(err))
		}
		close(workCh)
		ih.wg.Done()
	}()
	return nil
}

// fetchHandles fetches a batch of handles from index data and builds the index lookup tasks.
func (ih *indexHandler) fetchHandles(result distsql.SelectResult, workCh chan<- *lookupTableTask, ctx goctx.Context, finished <-chan struct{}) {
	for {
		handles, finish, err := extractHandlesFromIndexResult(result)
		if err != nil {
			workCh <- &lookupTableTask{
				tasksErr: errors.Trace(err),
			}
			return
		}
		if finish {
			return
		}
		tasks := ih.buildTableTasks(handles)
		for _, task := range tasks {
			select {
			case <-ctx.Done():
				return
			case <-finished:
				return
			case workCh <- task:
			}
		}
	}
}

func (ih *indexHandler) close() {
	ih.wg.Wait()
}

// tableHandler is used by IndexLookUpExecutor to maintain table lookup background goroutines.
type tableHandler struct {
	taskChan    chan *lookupTableTask
	taskCurr    *lookupTableTask
	executeTask func(task *lookupTableTask, goCtx goctx.Context)
}

// open the tableHandler, launch some background worker goroutines which pick
// tasks from workCh, execute the task and store the results in tableHandler's taskChan.
func (th *tableHandler) open(e *IndexLookUpExecutor, workCh <-chan *lookupTableTask, finished <-chan struct{}) {
	th.executeTask = e.executeTask
	th.taskChan = make(chan *lookupTableTask, atomic.LoadInt32(&LookupTableTaskChannelSize))
	lookupConcurrencyLimit := e.ctx.GetSessionVars().IndexLookupConcurrency
	var wg sync.WaitGroup
	wg.Add(lookupConcurrencyLimit)
	for i := 0; i < lookupConcurrencyLimit; i++ {
		ctx, cancel := goctx.WithCancel(e.ctx.GoCtx())
		go func() {
			th.pickAndExecTask(workCh, ctx, finished)
			cancel()
			wg.Done()
		}()
	}
	go func() {
		wg.Wait()
		close(th.taskChan)
	}()
}

// pickAndExecTask picks tasks from workCh, execute them, and store the result to tableHandler's taskCh.
func (th *tableHandler) pickAndExecTask(workCh <-chan *lookupTableTask, ctx goctx.Context, finished <-chan struct{}) {
	for {
		select {
		case task, ok := <-workCh:
			if !ok {
				return
			}
			if task.tasksErr != nil {
				th.taskChan <- task
				return
			}

			// TODO: The results can be simplified when new_distsql.go replace distsql.go totally.
			th.executeTask(task, ctx)
			th.taskChan <- task
		case <-ctx.Done():
			return
		case <-finished:
			return
		}
	}
}

func (th *tableHandler) next() (Row, error) {
	for {
		if th.taskCurr == nil {
			taskCurr, ok := <-th.taskChan
			if !ok {
				return nil, nil
			}
			if taskCurr.tasksErr != nil {
				return nil, errors.Trace(taskCurr.tasksErr)
			}
			th.taskCurr = taskCurr
		}
		row, err := th.taskCurr.getRow()
		if err != nil {
			return nil, errors.Trace(err)
		}
		if row != nil {
			return row, nil
		}
		th.taskCurr = nil
	}
}

func (th *tableHandler) close() {
	for range th.taskChan {
	}
}

// Open implements the Executor Open interface.
func (e *IndexLookUpExecutor) Open() error {
	kvRanges, err := e.indexRangesToKVRanges()
	if err != nil {
		return errors.Trace(err)
	}
	return e.open(kvRanges)
}

func (e *IndexLookUpExecutor) open(kvRanges []kv.KeyRange) error {
	e.finished = make(chan struct{})
	e.indexHandler = indexHandler{}
	e.tableHandler = tableHandler{}

	// indexHandler will write to workCh and tableHandler will read from workCh,
	// so fetching index and getting table data can run concurrently.
	workCh := make(chan *lookupTableTask, 1)
	err := e.indexHandler.open(kvRanges, e, workCh, e.finished)
	if err != nil {
		return errors.Trace(err)
	}
	e.tableHandler.open(e, workCh, e.finished)
	return nil
}

func (e *IndexLookUpExecutor) indexRangesToKVRanges() ([]kv.KeyRange, error) {
	fieldTypes := make([]*types.FieldType, len(e.index.Columns))
	for i, v := range e.index.Columns {
		fieldTypes[i] = &(e.table.Cols()[v.Offset].FieldType)
	}
	return indexRangesToKVRanges(e.ctx.GetSessionVars().StmtCtx, e.tableID, e.index.ID, e.ranges, fieldTypes)
}

// doRequestForDatums constructs kv ranges by datums. It is used by index look up join.
func (e *IndexLookUpExecutor) doRequestForDatums(values [][]types.Datum, goCtx goctx.Context) error {
	kvRanges, err := indexValuesToKVRanges(e.tableID, e.index.ID, values)
	if err != nil {
		return errors.Trace(err)
	}
	return e.open(kvRanges)
}

// executeTask executes the table look up tasks. We will construct a table reader and send request by handles.
// Then we hold the returning rows and finish this task.
func (e *IndexLookUpExecutor) executeTask(task *lookupTableTask, goCtx goctx.Context) {
	var (
		err       error
		handleCol *expression.Column
	)
	schema := e.schema.Clone()
	defer func() {
		task.doneCh <- errors.Trace(err)
	}()
	if e.handleCol != nil {
		handleCol = e.handleCol
	} else if e.keepOrder {
		handleCol = &expression.Column{
			ID:    model.ExtraHandleID,
			Index: e.schema.Len(),
		}
		schema.Append(handleCol)
	}
	tableReader := &TableReaderExecutor{
		table:     e.table,
		tableID:   e.tableID,
		dagPB:     e.tableRequest,
		schema:    schema,
		ctx:       e.ctx,
		handleCol: handleCol,
	}
	err = tableReader.doRequestForHandles(task.handles, goCtx)
	if err != nil {
		return
	}
	defer tableReader.Close()
	for {
		var row Row
		row, err = tableReader.Next()
		if err != nil || row == nil {
			break
		}
		task.rows = append(task.rows, row)
	}
	if e.keepOrder {
		// Restore the index order.
		sorter := &rowsSorter{order: task.indexOrder, rows: task.rows, handleIdx: handleCol.Index}
		sort.Sort(sorter)
		if e.handleCol == nil {
			for i, row := range task.rows {
				task.rows[i] = row[:len(row)-1]
			}
		}
	}
}

func (e *IndexLookUpExecutor) buildTableTasks(handles []int64) []*lookupTableTask {
	// Build tasks with increasing batch size.
	var taskSizes []int
	total := len(handles)
	batchSize := e.ctx.GetSessionVars().IndexLookupSize
	for total > 0 {
		if batchSize > total {
			batchSize = total
		}
		taskSizes = append(taskSizes, batchSize)
		total -= batchSize
	}

	var indexOrder map[int64]int
	if e.keepOrder {
		// Save the index order.
		indexOrder = make(map[int64]int, len(handles))
		for i, h := range handles {
			indexOrder[h] = i
		}
	}

	tasks := make([]*lookupTableTask, len(taskSizes))
	for i, size := range taskSizes {
		task := &lookupTableTask{
			handles:    handles[:size],
			indexOrder: indexOrder,
		}
		task.doneCh = make(chan error, 1)
		handles = handles[size:]
		tasks[i] = task
	}
	return tasks
}

// Schema implements Exec Schema interface.
func (e *IndexLookUpExecutor) Schema() *expression.Schema {
	return e.schema
}

// Close implements Exec Close interface.
func (e *IndexLookUpExecutor) Close() error {
	if e.finished != nil {
		close(e.finished)
		e.indexHandler.close()
		e.tableHandler.close()
		e.finished = nil
	}
	return nil
}

// Next implements Exec Next interface.
func (e *IndexLookUpExecutor) Next() (Row, error) {
	return e.tableHandler.next()
}
