// Copyright 2019 PingCAP, Inc.
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

package infoschema

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/meta/autoid"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/set"
	"golang.org/x/exp/slices"
)

const (
	promQLQuantileKey       = "$QUANTILE"
	promQLLabelConditionKey = "$LABEL_CONDITIONS"
	promQRangeDurationKey   = "$RANGE_DURATION"
)

func init() {
	// Initialize the metric schema database and register the driver to `drivers`.
	dbID := autoid.MetricSchemaDBID
	tableID := dbID + 1
	metricTables := make([]*model.TableInfo, 0, len(MetricTableMap))
	for name, def := range MetricTableMap {
		cols := def.genColumnInfos()
		tableInfo := buildTableMeta(name, cols)
		tableInfo.ID = tableID
		tableInfo.Comment = def.Comment
		tableID++
		metricTables = append(metricTables, tableInfo)
		tableInfo.MaxColumnID = int64(len(tableInfo.Columns))
		tableInfo.MaxIndexID = int64(len(tableInfo.Indices))
	}
	dbInfo := &model.DBInfo{
		ID:      dbID,
		Name:    model.NewCIStr(util.MetricSchemaName.O),
		Charset: mysql.DefaultCharset,
		Collate: mysql.DefaultCollationName,
		Tables:  metricTables,
	}
	RegisterVirtualTable(dbInfo, tableFromMeta)
}

// MetricTableDef is the metric table define.
type MetricTableDef struct {
	PromQL   string
	Labels   []string
	Quantile float64
	Comment  string
}

// IsMetricTable uses to checks whether the table is a metric table.
func IsMetricTable(lowerTableName string) bool {
	_, ok := MetricTableMap[lowerTableName]
	return ok
}

// GetMetricTableDef gets the metric table define.
func GetMetricTableDef(lowerTableName string) (*MetricTableDef, error) {
	def, ok := MetricTableMap[lowerTableName]
	if !ok {
		return nil, errors.Errorf("can not find metric table: %v", lowerTableName)
	}
	return &def, nil
}

func (def *MetricTableDef) genColumnInfos() []columnInfo {
	cols := []columnInfo{
		{name: "time", tp: mysql.TypeDatetime, size: 19, deflt: "CURRENT_TIMESTAMP"},
	}
	for _, label := range def.Labels {
		cols = append(cols, columnInfo{name: label, tp: mysql.TypeVarchar, size: 512})
	}
	if def.Quantile > 0 {
		defaultValue := strconv.FormatFloat(def.Quantile, 'f', -1, 64)
		cols = append(cols, columnInfo{name: "quantile", tp: mysql.TypeDouble, size: 22, deflt: defaultValue})
	}
	cols = append(cols, columnInfo{name: "value", tp: mysql.TypeDouble, size: 22})
	return cols
}

// GenPromQL generates the promQL.
func (def *MetricTableDef) GenPromQL(sctx sessionctx.Context, labels map[string]set.StringSet, quantile float64) string {
	promQL := def.PromQL
	promQL = strings.ReplaceAll(promQL, promQLQuantileKey, strconv.FormatFloat(quantile, 'f', -1, 64))
	promQL = strings.ReplaceAll(promQL, promQLLabelConditionKey, def.genLabelCondition(labels))
	promQL = strings.ReplaceAll(promQL, promQRangeDurationKey, strconv.FormatInt(sctx.GetSessionVars().MetricSchemaRangeDuration, 10)+"s")
	return promQL
}

func (def *MetricTableDef) genLabelCondition(labels map[string]set.StringSet) string {
	var buf bytes.Buffer
	index := 0
	for _, label := range def.Labels {
		values := labels[label]
		if len(values) == 0 {
			continue
		}
		if index > 0 {
			buf.WriteByte(',')
		}
		switch len(values) {
		case 1:
			buf.WriteString(fmt.Sprintf("%s=\"%s\"", label, GenLabelConditionValues(values)))
		default:
			buf.WriteString(fmt.Sprintf("%s=~\"%s\"", label, GenLabelConditionValues(values)))
		}
		index++
	}
	return buf.String()
}

// GenLabelConditionValues generates the label condition values.
func GenLabelConditionValues(values set.StringSet) string {
	vs := make([]string, 0, len(values))
	for k := range values {
		vs = append(vs, k)
	}
	slices.Sort(vs)
	return strings.Join(vs, "|")
}

// metricSchemaTable stands for the fake table all its data is in the memory.
type metricSchemaTable struct {
	infoschemaTable
}

func tableFromMeta(alloc autoid.Allocators, meta *model.TableInfo) (table.Table, error) {
	columns := make([]*table.Column, 0, len(meta.Columns))
	for _, colInfo := range meta.Columns {
		col := table.ToColumn(colInfo)
		columns = append(columns, col)
	}
	t := &metricSchemaTable{
		infoschemaTable: infoschemaTable{
			meta: meta,
			cols: columns,
			tp:   table.VirtualTable,
		},
	}
	return t, nil
}
