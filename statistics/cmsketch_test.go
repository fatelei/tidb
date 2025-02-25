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
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package statistics

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/codec"
	"github.com/stretchr/testify/require"
)

func (c *CMSketch) insert(val *types.Datum) error {
	bytes, err := codec.EncodeValue(nil, nil, *val)
	if err != nil {
		return errors.Trace(err)
	}
	c.InsertBytes(bytes)
	return nil
}

func prepareCMSAndTopN(d, w int32, vals []*types.Datum, n uint32, total uint64) (*CMSketch, *TopN, error) {
	data := make([][]byte, 0, len(vals))
	for _, v := range vals {
		bytes, err := codec.EncodeValue(nil, nil, *v)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		data = append(data, bytes)
	}
	cms, topN, _, _ := NewCMSketchAndTopN(d, w, data, n, total)
	return cms, topN, nil
}

// buildCMSketchAndMapWithOffset builds cm sketch using zipf and the generated values starts from `offset`.
func buildCMSketchAndMapWithOffset(d, w int32, seed int64, total, imax uint64, s float64, offset int64) (*CMSketch, map[int64]uint32, error) {
	cms := NewCMSketch(d, w)
	mp := make(map[int64]uint32)
	zipf := rand.NewZipf(rand.New(rand.NewSource(seed)), s, 1, imax)
	for i := uint64(0); i < total; i++ {
		val := types.NewIntDatum(int64(zipf.Uint64()) + offset)
		err := cms.insert(&val)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		mp[val.GetInt64()]++
	}
	return cms, mp, nil
}

func buildCMSketchAndMap(d, w int32, seed int64, total, imax uint64, s float64) (*CMSketch, map[int64]uint32, error) {
	return buildCMSketchAndMapWithOffset(d, w, seed, total, imax, s, 0)
}

func buildCMSketchTopNAndMap(d, w, n, sample int32, seed int64, total, imax uint64, s float64) (*CMSketch, *TopN, map[int64]uint32, error) {
	mp := make(map[int64]uint32)
	zipf := rand.NewZipf(rand.New(rand.NewSource(seed)), s, 1, imax)
	vals := make([]*types.Datum, 0)
	for i := uint64(0); i < total; i++ {
		val := types.NewIntDatum(int64(zipf.Uint64()))
		mp[val.GetInt64()]++
		if i < uint64(sample) {
			vals = append(vals, &val)
		}
	}
	cms, topN, err := prepareCMSAndTopN(d, w, vals, uint32(n), total)
	return cms, topN, mp, err
}

func averageAbsoluteError(cms *CMSketch, topN *TopN, mp map[int64]uint32) (uint64, error) {
	var total uint64
	for num, count := range mp {
		estimate, err := queryValue(nil, cms, topN, types.NewIntDatum(num))
		if err != nil {
			return 0, errors.Trace(err)
		}
		var diff uint64
		if uint64(count) > estimate {
			diff = uint64(count) - estimate
		} else {
			diff = estimate - uint64(count)
		}
		total += diff
	}
	return total / uint64(len(mp)), nil
}

func TestCMSketch(t *testing.T) {
	tests := []struct {
		zipfFactor float64
		avgError   uint64
	}{
		{
			zipfFactor: 1.1,
			avgError:   3,
		},
		{
			zipfFactor: 2,
			avgError:   24,
		},
		{
			zipfFactor: 3,
			avgError:   63,
		},
	}
	d, w := int32(5), int32(2048)
	total, imax := uint64(100000), uint64(1000000)
	for _, tt := range tests {
		lSketch, lMap, err := buildCMSketchAndMap(d, w, 0, total, imax, tt.zipfFactor)
		require.NoError(t, err)
		avg, err := averageAbsoluteError(lSketch, nil, lMap)
		require.NoError(t, err)
		require.LessOrEqual(t, avg, tt.avgError)

		rSketch, rMap, err := buildCMSketchAndMap(d, w, 1, total, imax, tt.zipfFactor)
		require.NoError(t, err)
		avg, err = averageAbsoluteError(rSketch, nil, rMap)
		require.NoError(t, err)
		require.LessOrEqual(t, avg, tt.avgError)

		err = lSketch.MergeCMSketch(rSketch)
		require.NoError(t, err)
		for val, count := range rMap {
			lMap[val] += count
		}
		avg, err = averageAbsoluteError(lSketch, nil, lMap)
		require.NoError(t, err)
		require.Less(t, avg, tt.avgError*2)
	}
}

func TestCMSketchCoding(t *testing.T) {
	lSketch := NewCMSketch(5, 2048)
	lSketch.count = 2048 * math.MaxUint32
	for i := range lSketch.table {
		for j := range lSketch.table[i] {
			lSketch.table[i][j] = math.MaxUint32
		}
	}
	bytes, err := EncodeCMSketchWithoutTopN(lSketch)
	require.NoError(t, err)
	require.Len(t, bytes, 61457)
	rSketch, _, err := DecodeCMSketchAndTopN(bytes, nil)
	require.NoError(t, err)
	require.True(t, lSketch.Equal(rSketch))
}

func TestCMSketchTopN(t *testing.T) {
	tests := []struct {
		zipfFactor float64
		avgError   uint64
	}{
		// If no significant most items, TopN may will produce results worse than normal algorithm.
		// The first two tests produces almost same avg.
		{
			zipfFactor: 1.0000001,
			avgError:   30,
		},
		{
			zipfFactor: 1.1,
			avgError:   30,
		},
		{
			zipfFactor: 2,
			avgError:   89,
		},
		// If the most data lies in a narrow range, our guess may have better result.
		// The error mainly comes from huge numbers.
		{
			zipfFactor: 5,
			avgError:   208,
		},
	}
	d, w := int32(5), int32(2048)
	total, imax := uint64(1000000), uint64(1000000)
	for _, tt := range tests {
		lSketch, topN, lMap, err := buildCMSketchTopNAndMap(d, w, 20, 1000, 0, total, imax, tt.zipfFactor)
		require.NoError(t, err)
		require.LessOrEqual(t, len(topN.TopN), 40)
		avg, err := averageAbsoluteError(lSketch, topN, lMap)
		require.NoError(t, err)
		require.LessOrEqual(t, avg, tt.avgError)
	}
}

func TestMergeCMSketch4IncrementalAnalyze(t *testing.T) {
	tests := []struct {
		zipfFactor float64
		avgError   uint64
	}{
		{
			zipfFactor: 1.0000001,
			avgError:   48,
		},
		{
			zipfFactor: 1.1,
			avgError:   48,
		},
		{
			zipfFactor: 2,
			avgError:   128,
		},
		{
			zipfFactor: 5,
			avgError:   256,
		},
	}
	d, w := int32(5), int32(2048)
	total, imax := uint64(100000), uint64(1000000)
	for _, tt := range tests {
		lSketch, lMap, err := buildCMSketchAndMap(d, w, 0, total, imax, tt.zipfFactor)
		require.NoError(t, err)
		avg, err := averageAbsoluteError(lSketch, nil, lMap)
		require.NoError(t, err)
		require.LessOrEqual(t, avg, tt.avgError)

		rSketch, rMap, err := buildCMSketchAndMapWithOffset(d, w, 1, total, imax, tt.zipfFactor, int64(imax))
		require.NoError(t, err)
		avg, err = averageAbsoluteError(rSketch, nil, rMap)
		require.NoError(t, err)
		require.LessOrEqual(t, avg, tt.avgError)

		for key, val := range rMap {
			lMap[key] += val
		}
		require.NoError(t, lSketch.MergeCMSketch4IncrementalAnalyze(rSketch, 0))
		avg, err = averageAbsoluteError(lSketch, nil, lMap)
		require.NoError(t, err)
		require.LessOrEqual(t, avg, tt.avgError)
		width, depth := lSketch.GetWidthAndDepth()
		require.Equal(t, int32(2048), width)
		require.Equal(t, int32(5), depth)
	}
}

func TestCMSketchTopNUniqueData(t *testing.T) {
	d, w := int32(5), int32(2048)
	total := uint64(1000000)
	mp := make(map[int64]uint32)
	vals := make([]*types.Datum, 0)
	for i := uint64(0); i < total; i++ {
		val := types.NewIntDatum(int64(i))
		mp[val.GetInt64()]++
		if i < uint64(1000) {
			vals = append(vals, &val)
		}
	}
	cms, topN, err := prepareCMSAndTopN(d, w, vals, uint32(20), total)
	require.NoError(t, err)
	avg, err := averageAbsoluteError(cms, topN, mp)
	require.NoError(t, err)
	require.Equal(t, uint64(1), cms.defaultValue)
	require.Equal(t, uint64(0), avg)
	require.Nil(t, topN)
}

func TestCMSketchCodingTopN(t *testing.T) {
	lSketch := NewCMSketch(5, 2048)
	lSketch.count = 2048 * (math.MaxUint32)
	for i := range lSketch.table {
		for j := range lSketch.table[i] {
			lSketch.table[i][j] = math.MaxUint32
		}
	}
	topN := make([]TopNMeta, 20)
	unsignedLong := types.NewFieldType(mysql.TypeLonglong)
	unsignedLong.AddFlag(mysql.UnsignedFlag)
	chk := chunk.New([]*types.FieldType{types.NewFieldType(mysql.TypeBlob), unsignedLong}, 20, 20)
	var rows []chunk.Row
	for i := 0; i < 20; i++ {
		tString := []byte(fmt.Sprintf("%20000d", i))
		topN[i] = TopNMeta{tString, math.MaxUint64}
		chk.AppendBytes(0, tString)
		chk.AppendUint64(1, math.MaxUint64)
		rows = append(rows, chk.GetRow(i))
	}

	bytes, err := EncodeCMSketchWithoutTopN(lSketch)
	require.NoError(t, err)
	require.Len(t, bytes, 61457)
	rSketch, _, err := DecodeCMSketchAndTopN(bytes, rows)
	require.NoError(t, err)
	require.True(t, lSketch.Equal(rSketch))
	// do not panic
	_, _, err = DecodeCMSketchAndTopN([]byte{}, rows)
	require.NoError(t, err)
}

func TestMergePartTopN2GlobalTopNWithoutHists(t *testing.T) {
	loc := time.UTC
	sc := &stmtctx.StatementContext{TimeZone: loc}
	version := 1
	isKilled := uint32(0)

	// Prepare TopNs.
	topNs := make([]*TopN, 0, 10)
	for i := 0; i < 10; i++ {
		// Construct TopN, should be key(1, 1) -> 2, key(1, 2) -> 2, key(1, 3) -> 3.
		topN := NewTopN(3)
		{
			key1, err := codec.EncodeKey(sc, nil, types.NewIntDatum(1), types.NewIntDatum(1))
			require.NoError(t, err)
			topN.AppendTopN(key1, 2)
			key2, err := codec.EncodeKey(sc, nil, types.NewIntDatum(1), types.NewIntDatum(2))
			require.NoError(t, err)
			topN.AppendTopN(key2, 2)
			key3, err := codec.EncodeKey(sc, nil, types.NewIntDatum(1), types.NewIntDatum(3))
			require.NoError(t, err)
			topN.AppendTopN(key3, 3)
		}
		topNs = append(topNs, topN)
	}

	// Test merge 2 topN with nil hists.
	globalTopN, leftTopN, _, err := MergePartTopN2GlobalTopN(loc, version, topNs, 2, nil, false, &isKilled)
	require.NoError(t, err)
	require.Len(t, globalTopN.TopN, 2, "should only have 2 topN")
	require.Equal(t, uint64(50), globalTopN.TotalCount(), "should have 50 rows")
	require.Len(t, leftTopN, 1, "should have 1 left topN")
}

func TestMergePartTopN2GlobalTopNWithHists(t *testing.T) {
	loc := time.UTC
	sc := &stmtctx.StatementContext{TimeZone: loc}
	version := 1
	isKilled := uint32(0)

	// Prepare TopNs.
	topNs := make([]*TopN, 0, 10)
	for i := 0; i < 10; i++ {
		// Construct TopN, should be key1 -> 2, key2 -> 2, key3 -> 3.
		topN := NewTopN(3)
		{
			key1, err := codec.EncodeKey(sc, nil, types.NewIntDatum(1))
			require.NoError(t, err)
			topN.AppendTopN(key1, 2)
			key2, err := codec.EncodeKey(sc, nil, types.NewIntDatum(2))
			require.NoError(t, err)
			topN.AppendTopN(key2, 2)
			if i%2 == 0 {
				key3, err := codec.EncodeKey(sc, nil, types.NewIntDatum(3))
				require.NoError(t, err)
				topN.AppendTopN(key3, 3)
			}
		}
		topNs = append(topNs, topN)
	}

	// Prepare Hists.
	hists := make([]*Histogram, 0, 10)
	for i := 0; i < 10; i++ {
		// Construct Hist
		h := NewHistogram(1, 10, 0, 0, types.NewFieldType(mysql.TypeTiny), chunk.InitialCapacity, 0)
		h.Bounds.AppendInt64(0, 1)
		h.Buckets = append(h.Buckets, Bucket{Repeat: 10, Count: 20})
		h.Bounds.AppendInt64(0, 2)
		h.Buckets = append(h.Buckets, Bucket{Repeat: 10, Count: 30})
		h.Bounds.AppendInt64(0, 3)
		h.Buckets = append(h.Buckets, Bucket{Repeat: 10, Count: 30})
		h.Bounds.AppendInt64(0, 4)
		h.Buckets = append(h.Buckets, Bucket{Repeat: 10, Count: 40})
		hists = append(hists, h)
	}

	// Test merge 2 topN.
	globalTopN, leftTopN, _, err := MergePartTopN2GlobalTopN(loc, version, topNs, 2, hists, false, &isKilled)
	require.NoError(t, err)
	require.Len(t, globalTopN.TopN, 2, "should only have 2 topN")
	require.Equal(t, uint64(55), globalTopN.TotalCount(), "should have 55")
	require.Len(t, leftTopN, 1, "should have 1 left topN")
}

// cmd: go test -run=^$ -bench=BenchmarkMergePartTopN2GlobalTopNWithHists -benchmem github.com/pingcap/tidb/statistics
func benchmarkMergePartTopN2GlobalTopNWithHists(partitions int, b *testing.B) {
	loc := time.UTC
	sc := &stmtctx.StatementContext{TimeZone: loc}
	version := 1
	isKilled := uint32(0)

	// Prepare TopNs.
	topNs := make([]*TopN, 0, partitions)
	for i := 0; i < partitions; i++ {
		// Construct TopN, should be key1 -> 2, key2 -> 2, key3 -> 3.
		topN := NewTopN(3)
		{
			key1, err := codec.EncodeKey(sc, nil, types.NewIntDatum(1))
			require.NoError(b, err)
			topN.AppendTopN(key1, 2)
			key2, err := codec.EncodeKey(sc, nil, types.NewIntDatum(2))
			require.NoError(b, err)
			topN.AppendTopN(key2, 2)
			if i%2 == 0 {
				key3, err := codec.EncodeKey(sc, nil, types.NewIntDatum(3))
				require.NoError(b, err)
				topN.AppendTopN(key3, 3)
			}
		}
		topNs = append(topNs, topN)
	}

	// Prepare Hists.
	hists := make([]*Histogram, 0, partitions)
	for i := 0; i < partitions; i++ {
		// Construct Hist
		h := NewHistogram(1, 10, 0, 0, types.NewFieldType(mysql.TypeTiny), chunk.InitialCapacity, 0)
		h.Bounds.AppendInt64(0, 1)
		h.Buckets = append(h.Buckets, Bucket{Repeat: 10, Count: 20})
		h.Bounds.AppendInt64(0, 2)
		h.Buckets = append(h.Buckets, Bucket{Repeat: 10, Count: 30})
		h.Bounds.AppendInt64(0, 3)
		h.Buckets = append(h.Buckets, Bucket{Repeat: 10, Count: 30})
		h.Bounds.AppendInt64(0, 4)
		h.Buckets = append(h.Buckets, Bucket{Repeat: 10, Count: 40})
		hists = append(hists, h)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Benchmark merge 10 topN.
		_, _, _, _ = MergePartTopN2GlobalTopN(loc, version, topNs, 10, hists, false, &isKilled)
	}
}

func BenchmarkMergePartTopN2GlobalTopNWithHists100(b *testing.B) {
	benchmarkMergePartTopN2GlobalTopNWithHists(100, b)
}

func BenchmarkMergePartTopN2GlobalTopNWithHists1000(b *testing.B) {
	benchmarkMergePartTopN2GlobalTopNWithHists(1000, b)
}

func BenchmarkMergePartTopN2GlobalTopNWithHists10000(b *testing.B) {
	benchmarkMergePartTopN2GlobalTopNWithHists(10000, b)
}

func BenchmarkMergePartTopN2GlobalTopNWithHists100000(b *testing.B) {
	benchmarkMergePartTopN2GlobalTopNWithHists(100000, b)
}

func BenchmarkMergePartTopN2GlobalTopNWithHists1000000(b *testing.B) {
	benchmarkMergePartTopN2GlobalTopNWithHists(1000000, b)
}

func BenchmarkMergePartTopN2GlobalTopNWithHists10000000(b *testing.B) {
	benchmarkMergePartTopN2GlobalTopNWithHists(10000000, b)
}
