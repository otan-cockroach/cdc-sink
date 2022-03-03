// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package stage

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/cockroachdb/cdc-sink/internal/target/sinktest"
	"github.com/cockroachdb/cdc-sink/internal/types"
	"github.com/cockroachdb/cdc-sink/internal/util/batches"
	"github.com/cockroachdb/cdc-sink/internal/util/hlc"
	"github.com/cockroachdb/cdc-sink/internal/util/ident"
	"github.com/stretchr/testify/assert"
)

// TestPutAndDrain will insert and dequeue a batch of Mutations.
func TestPutAndDrain(t *testing.T) {
	a := assert.New(t)
	ctx, dbInfo, cancel := sinktest.Context()
	a.NotEmpty(dbInfo.Version())
	defer cancel()

	targetDB, cancel, err := sinktest.CreateDB(ctx)
	if !a.NoError(err) {
		return
	}
	defer cancel()

	factory := NewStagers(dbInfo.Pool(), ident.StagingDB)

	dummyTarget := ident.NewTable(
		targetDB, ident.Public, ident.New("target"))

	s, err := factory.Get(ctx, dummyTarget)
	if !a.NoError(err) {
		return
	}
	a.NotNil(s)

	stagingTable := s.(*stage).stage

	// Cook test data.
	total := 3 * batches.Size()
	muts := make([]types.Mutation, total)
	for i := range muts {
		muts[i] = types.Mutation{
			Data: []byte(fmt.Sprintf(`{"pk": %d}`, i)),
			Key:  []byte(fmt.Sprintf(`[%d]`, i)),
			Time: hlc.New(int64(1000*i), i),
		}
	}

	// Insert.
	a.NoError(s.Store(ctx, dbInfo.Pool(), muts))

	// Sanity-check table.
	count, err := sinktest.GetRowCount(ctx, dbInfo.Pool(), stagingTable)
	a.NoError(err)
	a.Equal(total, count)

	// Ensure that data insertion is idempotent.
	a.NoError(s.Store(ctx, dbInfo.Pool(), muts))

	// Sanity-check table.
	count, err = sinktest.GetRowCount(ctx, dbInfo.Pool(), stagingTable)
	a.NoError(err)
	a.Equal(total, count)

	// Dequeue.
	ret, err := s.Drain(ctx,
		dbInfo.Pool(),
		hlc.Zero(),
		hlc.New(int64(1000*total+1), 0),
	)
	a.NoError(err)
	a.Len(ret, total)

	// Should be empty now.
	count, err = sinktest.GetRowCount(ctx, dbInfo.Pool(), stagingTable)
	a.NoError(err)
	a.Equal(0, count)
}

func BenchmarkStage(b *testing.B) {
	sizes := []int{
		1,
		10,
		100,
		1000,
		10000,
	}
	for _, size := range sizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			benchmarkStage(b, size)
		})
	}
}

func benchmarkStage(b *testing.B, batchSize int) {
	ctx, dbInfo, cancel := sinktest.Context()
	defer cancel()

	targetDB, cancel, err := sinktest.CreateDB(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer cancel()

	factory := NewStagers(dbInfo.Pool(), ident.StagingDB)

	dummyTarget := ident.NewTable(
		targetDB, ident.Public, ident.New("target"))

	s, err := factory.Get(ctx, dummyTarget)
	if err != nil {
		b.Fatal(err)
	}

	allBytes := int64(0)
	muts := sinktest.MutationGenerator(ctx, 100000, 0.5)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		batch := make([]types.Mutation, batchSize)
		for pb.Next() {
			for i := range batch {
				mut := <-muts
				batch[i] = mut
				atomic.AddInt64(&allBytes, int64(len(mut.Data)+len(mut.Key)))
			}
			if err := s.Store(ctx, dbInfo.Pool(), batch); err != nil {
				b.Fatal(err)
			}
		}
	})
	// Use JSON byte count as throughput measure.
	b.SetBytes(atomic.LoadInt64(&allBytes))

}