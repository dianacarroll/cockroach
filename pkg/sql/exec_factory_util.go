// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/exec"
	"github.com/cockroachdb/cockroach/pkg/sql/rowexec"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/errors"
)

func constructPlan(
	planner *planner,
	root exec.Node,
	subqueries []exec.Subquery,
	cascades []exec.Cascade,
	checks []exec.Node,
) (exec.Plan, error) {
	res := &planTop{
		// TODO(radu): these fields can be modified by planning various opaque
		// statements. We should have a cleaner way of plumbing these.
		avoidBuffering:  planner.curPlan.avoidBuffering,
		auditEvents:     planner.curPlan.auditEvents,
		instrumentation: planner.curPlan.instrumentation,
	}
	assignPlan := func(plan *planMaybePhysical, node exec.Node) {
		switch n := node.(type) {
		case planNode:
			plan.planNode = n
		case planMaybePhysical:
			*plan = n
		default:
			panic(errors.AssertionFailedf("unexpected node type %T", node))
		}
	}
	assignPlan(&res.main, root)
	if len(subqueries) > 0 {
		res.subqueryPlans = make([]subquery, len(subqueries))
		for i := range subqueries {
			in := &subqueries[i]
			out := &res.subqueryPlans[i]
			out.subquery = in.ExprNode
			switch in.Mode {
			case exec.SubqueryExists:
				out.execMode = rowexec.SubqueryExecModeExists
			case exec.SubqueryOneRow:
				out.execMode = rowexec.SubqueryExecModeOneRow
			case exec.SubqueryAnyRows:
				out.execMode = rowexec.SubqueryExecModeAllRowsNormalized
			case exec.SubqueryAllRows:
				out.execMode = rowexec.SubqueryExecModeAllRows
			default:
				return nil, errors.Errorf("invalid SubqueryMode %d", in.Mode)
			}
			out.expanded = true
			assignPlan(&out.plan, in.Root)
		}
	}
	if len(cascades) > 0 {
		res.cascades = make([]cascadeMetadata, len(cascades))
		for i := range cascades {
			res.cascades[i].Cascade = cascades[i]
		}
	}
	if len(checks) > 0 {
		res.checkPlans = make([]checkPlan, len(checks))
		for i := range checks {
			assignPlan(&res.checkPlans[i].plan, checks[i])
		}
	}

	return res, nil
}

// makeScanColumnsConfig builds a scanColumnsConfig struct by constructing a
// list of descriptor IDs for columns in the given cols set. Columns are
// identified by their ordinal position in the table schema.
func makeScanColumnsConfig(table cat.Table, cols exec.TableColumnOrdinalSet) scanColumnsConfig {
	// Set visibility=execinfra.ScanVisibilityPublicAndNotPublic, since all
	// columns in the "cols" set should be projected, regardless of whether
	// they're public or non-public. The caller decides which columns to
	// include (or not include). Note that when wantedColumns is non-empty,
	// the visibility flag will never trigger the addition of more columns.
	colCfg := scanColumnsConfig{
		wantedColumns: make([]tree.ColumnID, 0, cols.Len()),
		visibility:    execinfra.ScanVisibilityPublicAndNotPublic,
	}
	for c, ok := cols.Next(0); ok; c, ok = cols.Next(c + 1) {
		desc := table.Column(c).(*descpb.ColumnDescriptor)
		colCfg.wantedColumns = append(colCfg.wantedColumns, tree.ColumnID(desc.ID))
	}
	return colCfg
}

func constructExplainPlanNode(
	options *tree.ExplainOptions, stmtType tree.StatementType, p *planTop, planner *planner,
) (exec.Node, error) {
	analyzeSet := options.Flags[tree.ExplainFlagAnalyze]

	if options.Flags[tree.ExplainFlagEnv] {
		return nil, errors.New("ENV only supported with (OPT) option")
	}

	switch options.Mode {
	case tree.ExplainDistSQL:
		return &explainDistSQLNode{
			options:  options,
			plan:     p.planComponents,
			analyze:  analyzeSet,
			stmtType: stmtType,
		}, nil

	case tree.ExplainVec:
		return &explainVecNode{
			options: options,
			plan:    p.planComponents,
		}, nil

	case tree.ExplainPlan:
		if analyzeSet {
			return nil, errors.New("EXPLAIN ANALYZE only supported with (DISTSQL) option")
		}
		return planner.makeExplainPlanNodeWithPlan(
			context.TODO(),
			options,
			p.planComponents,
		)

	default:
		panic(errors.AssertionFailedf("unsupported explain mode %v", options.Mode))
	}
}

// getResultColumnsForSimpleProject populates result columns for a simple
// projection. It supports two configurations:
// 1. colNames and resultTypes are non-nil. resultTypes indicates the updated
//    types (after the projection has been applied)
// 2. if colNames is nil, then inputCols must be non-nil (which are the result
//    columns before the projection has been applied).
func getResultColumnsForSimpleProject(
	cols []exec.NodeColumnOrdinal,
	colNames []string,
	resultTypes []*types.T,
	inputCols sqlbase.ResultColumns,
) sqlbase.ResultColumns {
	resultCols := make(sqlbase.ResultColumns, len(cols))
	for i, col := range cols {
		if colNames == nil {
			resultCols[i] = inputCols[col]
			// If we have a SimpleProject, we should clear the hidden bit on any
			// column since it indicates it's been explicitly selected.
			resultCols[i].Hidden = false
		} else {
			resultCols[i] = sqlbase.ResultColumn{
				Name: colNames[i],
				Typ:  resultTypes[i],
			}
		}
	}
	return resultCols
}

func getEqualityIndicesAndMergeJoinOrdering(
	leftOrdering, rightOrdering sqlbase.ColumnOrdering,
) (
	leftEqualityIndices, rightEqualityIndices []exec.NodeColumnOrdinal,
	mergeJoinOrdering sqlbase.ColumnOrdering,
	err error,
) {
	n := len(leftOrdering)
	if n == 0 || len(rightOrdering) != n {
		return nil, nil, nil, errors.Errorf(
			"orderings from the left and right side must be the same non-zero length",
		)
	}
	leftEqualityIndices = make([]exec.NodeColumnOrdinal, n)
	rightEqualityIndices = make([]exec.NodeColumnOrdinal, n)
	for i := 0; i < n; i++ {
		leftColIdx, rightColIdx := leftOrdering[i].ColIdx, rightOrdering[i].ColIdx
		leftEqualityIndices[i] = exec.NodeColumnOrdinal(leftColIdx)
		rightEqualityIndices[i] = exec.NodeColumnOrdinal(rightColIdx)
	}

	mergeJoinOrdering = make(sqlbase.ColumnOrdering, n)
	for i := 0; i < n; i++ {
		// The mergeJoinOrdering "columns" are equality column indices.  Because of
		// the way we constructed the equality indices, the ordering will always be
		// 0,1,2,3..
		mergeJoinOrdering[i].ColIdx = i
		mergeJoinOrdering[i].Direction = leftOrdering[i].Direction
	}
	return leftEqualityIndices, rightEqualityIndices, mergeJoinOrdering, nil
}

func getResultColumnsForGroupBy(
	inputCols sqlbase.ResultColumns, groupCols []exec.NodeColumnOrdinal, aggregations []exec.AggInfo,
) sqlbase.ResultColumns {
	columns := make(sqlbase.ResultColumns, 0, len(groupCols)+len(aggregations))
	for _, col := range groupCols {
		columns = append(columns, inputCols[col])
	}
	for _, agg := range aggregations {
		columns = append(columns, sqlbase.ResultColumn{
			Name: agg.FuncName,
			Typ:  agg.ResultType,
		})
	}
	return columns
}

// convertOrdinalsToInts converts a slice of exec.NodeColumnOrdinals to a slice
// of ints.
func convertOrdinalsToInts(ordinals []exec.NodeColumnOrdinal) []int {
	ints := make([]int, len(ordinals))
	for i := range ordinals {
		ints[i] = int(ordinals[i])
	}
	return ints
}

func constructVirtualScan(
	ef exec.Factory,
	p *planner,
	table cat.Table,
	index cat.Index,
	params exec.ScanParams,
	reqOrdering exec.OutputOrdering,
	// delayedNodeCallback is a callback function that performs custom setup
	// that varies by exec.Factory implementations.
	delayedNodeCallback func(*delayedNode) (exec.Node, error),
) (exec.Node, error) {
	tn := &table.(*optVirtualTable).name
	virtual, err := p.getVirtualTabler().getVirtualTableEntry(tn)
	if err != nil {
		return nil, err
	}
	indexDesc := index.(*optVirtualIndex).desc
	columns, constructor := virtual.getPlanInfo(
		table.(*optVirtualTable).desc,
		indexDesc, params.IndexConstraint)

	n, err := delayedNodeCallback(&delayedNode{
		name:            fmt.Sprintf("%s@%s", table.Name(), index.Name()),
		columns:         columns,
		indexConstraint: params.IndexConstraint,
		constructor: func(ctx context.Context, p *planner) (planNode, error) {
			return constructor(ctx, p, tn.Catalog())
		},
	})
	if err != nil {
		return nil, err
	}

	// Check for explicit use of the dummy column.
	if params.NeededCols.Contains(0) {
		return nil, errors.Errorf("use of %s column not allowed.", table.Column(0).ColName())
	}
	if params.Locking != nil {
		// We shouldn't have allowed SELECT FOR UPDATE for a virtual table.
		return nil, errors.AssertionFailedf("locking cannot be used with virtual table")
	}
	if needed := params.NeededCols; needed.Len() != len(columns) {
		// We are selecting a subset of columns; we need a projection.
		cols := make([]exec.NodeColumnOrdinal, 0, needed.Len())
		for ord, ok := needed.Next(0); ok; ord, ok = needed.Next(ord + 1) {
			cols = append(cols, exec.NodeColumnOrdinal(ord-1))
		}
		n, err = ef.ConstructSimpleProject(n, cols, nil /* reqOrdering */)
		if err != nil {
			return nil, err
		}
	}
	if params.HardLimit != 0 {
		n, err = ef.ConstructLimit(n, tree.NewDInt(tree.DInt(params.HardLimit)), nil /* offset */)
		if err != nil {
			return nil, err
		}
	}
	// reqOrdering will be set if the optimizer expects that the output of the
	// exec.Node that we're returning will actually have a legitimate ordering.
	// Virtual indexes never provide a legitimate ordering, so we have to make
	// sure to sort if we have a required ordering.
	if len(reqOrdering) != 0 {
		n, err = ef.ConstructSort(n, sqlbase.ColumnOrdering(reqOrdering), 0)
		if err != nil {
			return nil, err
		}
	}
	return n, nil
}

func collectSystemColumnsFromCfg(
	colCfg *scanColumnsConfig,
) (systemColumns []descpb.SystemColumnKind, systemColumnOrdinals []int) {
	for i, id := range colCfg.wantedColumns {
		sysColKind := sqlbase.GetSystemColumnKindFromColumnID(descpb.ColumnID(id))
		if sysColKind != descpb.SystemColumnKind_NONE {
			// The scan is requested to produce a system column.
			systemColumns = append(systemColumns, sysColKind)
			systemColumnOrdinals = append(systemColumnOrdinals, i)
		}
	}
	return systemColumns, systemColumnOrdinals
}

func constructOpaque(metadata opt.OpaqueMetadata) (planNode, error) {
	o, ok := metadata.(*opaqueMetadata)
	if !ok {
		return nil, errors.AssertionFailedf("unexpected OpaqueMetadata object type %T", metadata)
	}
	return o.plan, nil
}
