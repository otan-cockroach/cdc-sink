// Copyright 2023 The Cockroach Authors
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
//
// SPDX-License-Identifier: Apache-2.0

// Package stage defines a means of storing and retrieving mutations
// to be applied to a table.
package stage

// The code in this file is reworked from sink_table.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"time"

	"github.com/cockroachdb/field-eng-powertools/notify"
	"github.com/cockroachdb/field-eng-powertools/stopper"
	"github.com/cockroachdb/replicator/internal/types"
	"github.com/cockroachdb/replicator/internal/util/batches"
	"github.com/cockroachdb/replicator/internal/util/hlc"
	"github.com/cockroachdb/replicator/internal/util/ident"
	"github.com/cockroachdb/replicator/internal/util/metrics"
	"github.com/cockroachdb/replicator/internal/util/msort"
	"github.com/cockroachdb/replicator/internal/util/retry"
	"github.com/jackc/pgx/v5"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

// This mutation value will be stored when a mutation is marked as
// having been applied without having been previously staged.
var stubSentinel = json.RawMessage(`{"__stub__":true}`)

// stagingTable returns the staging table name that will store mutations
// for the given target table.
func stagingTable(stagingDB ident.Schema, target ident.Table) ident.Table {
	target = target.Canonical() // Use lower-cased version of the table.
	mangled := ident.Join(target, ident.Raw, '_')
	return ident.NewTable(stagingDB, ident.New(mangled))
}

// stage implements a storage and retrieval mechanism for staging
// Mutation instances.
type stage struct {
	cfg        *Config
	stage      *ident.Hinted[ident.Table] // The staging table that holds the mutations.
	stagingDB  *types.StagingPool
	retireFrom notify.Var[hlc.Time] // Makes subsequent calls to Retire() a bit faster.

	consistencyError prometheus.Gauge
	filterApplied    prometheus.Observer
	filterCount      prometheus.Counter
	markDuration     prometheus.Observer
	retireDuration   prometheus.Observer
	retireError      prometheus.Counter
	selectCount      prometheus.Counter
	selectDuration   prometheus.Observer
	selectError      prometheus.Counter
	staleCount       prometheus.Gauge
	stageCount       prometheus.Counter
	stageDupes       prometheus.Counter
	stageDuration    prometheus.Observer
	stageError       prometheus.Counter

	// Compute SQL fragments exactly once on startup.
	sql struct {
		filterApplied string // Select mutation keys that have been applied.
		markApplied   string // Mark mutations as having been applied.
		retire        string // Delete a batch of staged mutations.
		stage         string // General-purpose upsert into staging table.
		stageExists   string // Stage a mutation if one already exists.
		unapplied     string // Count stale, unapplied mutations.
		unappliedAOST string // Count stale, unapplied mutations.
	}
}

var _ types.Stager = (*stage)(nil)

const tableSchema = `
CREATE TABLE IF NOT EXISTS %[1]s (
       nanos INT NOT NULL,
     logical INT NOT NULL,
         key STRING NOT NULL,
         mut BYTES NOT NULL,
      before BYTES NULL,
     applied BOOL NOT NULL DEFAULT false,
  applied_at TIMESTAMPTZ NULL,
    deletion BOOL NOT NULL DEFAULT false,
  %[2]s
  PRIMARY KEY (nanos, logical, key),
    INDEX %[3]s (key) STORING (applied), -- Improve performance of StageIfExists
   FAMILY cold (mut, before, deletion),
   FAMILY hot (applied, applied_at)
)`

// newStage constructs a new mutation stage that will track pending
// mutations to be applied to the given target table.
func (f *factory) newStage(target ident.Table) (*stage, error) {
	ctx := f.stop
	table := stagingTable(f.stagingDB, target)
	keyIdx := ident.New(table.Table().Raw() + "_key_applied")
	// Try to create the staging table with a helper virtual column. We
	// never query for it, so it should have essentially no cost.
	if err := retry.Execute(ctx, f.db, fmt.Sprintf(tableSchema, table,
		`source_time TIMESTAMPTZ AS (to_timestamp(nanos::float/1e9)) VIRTUAL,`,
		keyIdx)); err != nil {

		// Old versions of CRDB don't know about to_timestamp(). Try
		// again without the helper column.
		if code, ok := f.db.ErrCode(err); ok && code == "42883" /* unknown function */ {
			err = retry.Execute(ctx, f.db, fmt.Sprintf(tableSchema, table, "", keyIdx))
		}
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	// Transparently upgrade older staging tables. This avoids needing
	// to add a breaking change to the Versions slice.
	log.Tracef("upgrading schema for %s", table)
	if err := retry.Execute(ctx, f.db, fmt.Sprintf(`
ALTER TABLE %[1]s ADD COLUMN IF NOT EXISTS before BYTES NULL
`, table)); err != nil {
		return nil, errors.WithStack(err)
	}
	if err := retry.Execute(ctx, f.db, fmt.Sprintf(`
CREATE INDEX IF NOT EXISTS %[1]s ON %[2]s (key) STORING (applied)
`, keyIdx, table)); err != nil {
		return nil, errors.WithStack(err)
	}
	// We're not going to worry about trying to backfill this, since
	// old, applied mutations are retired on a regular basis.
	if err := retry.Execute(ctx, f.db, fmt.Sprintf(`
ALTER TABLE %s ADD COLUMN IF NOT EXISTS applied_at TIMESTAMPTZ NULL
`, table)); err != nil {
		return nil, errors.WithStack(err)
	}
	if err := retry.Execute(ctx, f.db, fmt.Sprintf(`
ALTER TABLE %s ADD COLUMN IF NOT EXISTS deletion BOOL NULL
`, table)); err != nil {
		return nil, errors.WithStack(err)
	}
	log.Tracef("completed schema upgrades for %s", table)

	labels := metrics.TableValues(target)
	s := &stage{
		cfg:              f.cfg,
		stage:            f.db.HintNoFTS(table),
		stagingDB:        f.db,
		consistencyError: stageConsistencyErrors.WithLabelValues(labels...),
		filterApplied:    stageFilterAppliedDuration.WithLabelValues(labels...),
		filterCount:      stageFilterCount.WithLabelValues(labels...),
		markDuration:     stageMarkDuration.WithLabelValues(labels...),
		retireDuration:   stageRetireDurations.WithLabelValues(labels...),
		retireError:      stageRetireErrors.WithLabelValues(labels...),
		selectCount:      stageSelectCount.WithLabelValues(labels...),
		selectDuration:   stageSelectDurations.WithLabelValues(labels...),
		selectError:      stageSelectErrors.WithLabelValues(labels...),
		staleCount:       stageStaleMutations.WithLabelValues(labels...),
		stageCount:       stageCount.WithLabelValues(labels...),
		stageDupes:       stageDuplicateCount.WithLabelValues(labels...),
		stageDuration:    stageDuration.WithLabelValues(labels...),
		stageError:       stageErrors.WithLabelValues(labels...),
	}

	// Prevent these hot-path queries from being planned with a full
	// table scan if statistics are stale.
	tableHinted := f.db.HintNoFTS(table)
	s.sql.filterApplied = fmt.Sprintf(filterAppliedTemplate, tableHinted)
	s.sql.markApplied = fmt.Sprintf(markAppliedTemplate, tableHinted, stubSentinel)
	s.sql.retire = fmt.Sprintf(retireTemplate, tableHinted)
	s.sql.stage = fmt.Sprintf(stageTemplate, tableHinted)
	s.sql.stageExists = fmt.Sprintf(stageIfExistsTemplate, tableHinted)
	s.sql.unapplied = fmt.Sprintf(countTemplate, tableHinted, "")
	s.sql.unappliedAOST = fmt.Sprintf(countTemplate, tableHinted,
		"AS OF SYSTEM TIME follower_read_timestamp()")

	// Report unapplied mutations on a periodic basis.
	ctx.Go(func(ctx *stopper.Context) error {
		if f.cfg.UnappliedPeriod <= 0 {
			return nil
		}
		ticker := time.NewTicker(f.cfg.UnappliedPeriod)
		defer ticker.Stop()

		for {
			// We don't want to select on the notification channel,
			// since this value may be updated at a high rate on the
			// instance of Replicator that holds the resolver lease.
			from, _ := s.retireFrom.Get()
			ct, err := s.CountUnapplied(ctx, f.db, from, true /* AOST */)
			if code, ok := s.stagingDB.ErrCode(err); ok &&
				(code == "3D000" /* invalid_catalog_name */ ||
					code == "42P01" /* undefined_table */) {
				// This prevents log spam during testing or initial
				// startup, since the AOST query may push the read
				// behind the database time at which the table or
				// database was created.
			} else if err != nil {
				log.WithError(err).Warnf(
					"could not count unapplied mutations for target: %s", target)
			} else {
				s.staleCount.Set(float64(ct))
			}

			select {
			case <-ctx.Stopping():
				return nil
			case <-ticker.C:
				// Ensure that values get reset if this instance of
				// Replicator isn't the one that's actively resolving or
				// retiring mutations.
			}
		}
	})

	// Validate table consistency on a periodic basis.
	ctx.Go(func(ctx *stopper.Context) error {
		// Non-positive value to disable this behavior.
		if f.cfg.SanityCheckPeriod <= 0 {
			return nil
		}
		ticker := time.NewTicker(f.cfg.SanityCheckPeriod)
		defer ticker.Stop()
		for {
			ct, err := s.CheckConsistency(ctx, s.stagingDB, nil /* all keys */, true /* follower read */)
			if code, ok := s.stagingDB.ErrCode(err); ok &&
				(code == "3D000" /* invalid_catalog_name */ ||
					code == "42P01" /* undefined_table */) {
				// This prevents log spam during testing or initial
				// startup, since the AOST query may push the read
				// behind the database time at which the table or
				// database was created.
			} else if err != nil {
				log.WithError(err).Warn("could not check consistency")
			} else {
				s.consistencyError.Set(float64(ct))
			}

			select {
			case <-ctx.Stopping():
				return nil
			case <-ticker.C:
			}
		}
	})

	return s, nil
}

const countTemplate = `
SELECT count(*) FROM %s %s
WHERE (nanos, logical) < ($1, $2) AND NOT applied
`

// CountUnapplied returns the number of dangling mutations that likely
// indicate an error condition.
func (s *stage) CountUnapplied(
	ctx context.Context, db types.StagingQuerier, before hlc.Time, aost bool,
) (int, error) {
	var q string
	if aost {
		q = s.sql.unappliedAOST
	} else {
		q = s.sql.unapplied
	}

	var ret int
	err := retry.Retry(ctx, s.stagingDB, func(ctx context.Context) error {
		return db.QueryRow(ctx, q, before.Nanos(), before.Logical()).Scan(&ret)
	})
	return ret, errors.Wrap(err, q)
}

// This query returns the input indices of mutations that have already
// been applied.
//
//	$1 - key array
//	$2 - nanos array
//	$3 - logical array
const filterAppliedTemplate = `
WITH ids (idx, key, nanos, logical) AS (
  SELECT row_number() OVER (), unnest($1::STRING[]), unnest($2::INT[]), unnest($3::INT[]))
SELECT idx FROM ids
JOIN %s
USING (key, nanos, logical)
WHERE applied
`

// FilterApplied implements [types.Stager].
func (s *stage) FilterApplied(
	ctx context.Context, db types.StagingQuerier, muts []types.Mutation,
) ([]types.Mutation, error) {
	start := time.Now()

	keys := make([]string, len(muts))
	nanos := make([]int64, len(muts))
	logical := make([]int, len(muts))
	for idx, mut := range muts {
		keys[idx] = string(mut.Key)
		nanos[idx] = mut.Time.Nanos()
		logical[idx] = mut.Time.Logical()
	}

	rows, err := db.Query(ctx, s.sql.filterApplied, keys, nanos, logical)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	applied := make(map[int]bool, len(muts))
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			return nil, errors.WithStack(err)
		}
		// Returned indices are 1-based.
		applied[idx-1] = true
	}

	var ret []types.Mutation
	if len(applied) == 0 {
		// Typical case where none of the mutations have been applied.
		// Just make a fast copy of the slice.
		ret = make([]types.Mutation, len(muts))
		copy(ret, muts)
	} else {
		ret = make([]types.Mutation, 0, len(muts))
		for idx, mut := range muts {
			if !applied[idx] {
				ret = append(ret, mut)
			}
		}
		s.filterCount.Add(float64(len(muts) - len(ret)))
	}
	s.filterApplied.Observe(time.Since(start).Seconds())
	return ret, nil
}

// GetTable returns the table that the stage is storing into.
func (s *stage) GetTable() ident.Table { return s.stage.Base }

// The byte-array casts on $4 and $5 are because arrays of JSONB aren't implemented:
// https://github.com/cockroachdb/cockroach/issues/23468
const stageTemplate = `
INSERT INTO %s (nanos, logical, key, mut, before, deletion)
SELECT unnest($1::INT[]),
       unnest($2::INT[]),
       unnest($3::STRING[]),
       unnest($4::BYTES[]),
       unnest($5::BYTES[]),
       unnest($6::BOOL[])
ON CONFLICT DO NOTHING`

// Stage implements [types.Stager].
func (s *stage) Stage(
	ctx context.Context, db types.StagingQuerier, mutations []types.Mutation,
) error {
	start := time.Now()

	mutations = msort.UniqueByTimeKey(mutations)

	// If we're working with a pool, and not a transaction, we'll stage
	// the data in a concurrent manner.
	var err error
	if _, isPool := db.(*types.StagingPool); isPool {
		eg, errCtx := errgroup.WithContext(ctx)
		err = batches.Batch(len(mutations), func(begin, end int) error {
			eg.Go(func() error {
				return s.stageOneBatch(errCtx, db, mutations[begin:end])
			})
			return nil
		})
		if err != nil {
			return err
		}
		err = eg.Wait()
	} else {
		err = batches.Batch(len(mutations), func(begin, end int) error {
			return s.stageOneBatch(ctx, db, mutations[begin:end])
		})
	}

	if err != nil {
		s.stageError.Inc()
		return err
	}

	d := time.Since(start)
	s.stageCount.Add(float64(len(mutations)))
	s.stageDuration.Observe(d.Seconds())
	log.WithFields(log.Fields{
		"count":    len(mutations),
		"duration": d,
		"target":   s.stage,
	}).Debug("staged mutations")
	return nil
}

const stageIfExistsTemplate = `
WITH
proposed (idx, nanos, logical, key, mut, before, deletion) AS ( 
  SELECT 
    row_number() OVER (), 
    unnest($1::INT[]),
    unnest($2::INT[]),
    unnest($3::STRING[]),
    unnest($4::BYTES[]),
    unnest($5::BYTES[]),
    unnest($6::BOOL[])),
existing AS (
  SELECT DISTINCT proposed.key
  FROM proposed
  JOIN %[1]s existing
  ON (proposed.key = existing.key AND NOT existing.applied)),
action AS (
  UPSERT INTO %[1]s (nanos, logical, key, mut, before, deletion)
  SELECT nanos, logical, key, mut, before, deletion
  FROM proposed
  JOIN existing USING (key)
  RETURNING true) 
SELECT idx FROM proposed
JOIN existing USING (key)
`

// StageIfExists implements [types.Stager].
func (s *stage) StageIfExists(
	ctx context.Context, db types.StagingQuerier, mutations []types.Mutation,
) ([]types.Mutation, error) {
	nanos, logical, keys, jsons, befores, deletions, err := s.packArgs(ctx, mutations)
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(ctx, s.sql.stageExists,
		nanos, logical, keys, jsons, befores, deletions)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer rows.Close()

	staged := make(map[int]bool)
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			return nil, err
		}
		// The row counter is 1-based.
		staged[idx-1] = true
	}
	if err := rows.Err(); err != nil {
		return nil, errors.WithStack(err)
	}

	// We want to return a new slice and not mangle the input in case
	// the caller needs to re-use the input (e.g. FK retries).
	ret := make([]types.Mutation, 0, len(mutations)-len(staged))
	for idx, mut := range mutations {
		if !staged[idx] {
			ret = append(ret, mut)
		}
	}

	return ret, nil
}

// packArgs converts a slice of mutations into the various slices that
// we'll send to the staging database.
func (s *stage) packArgs(
	ctx context.Context, mutations []types.Mutation,
) (
	nanos []int64,
	logical []int,
	keys []string,
	jsons [][]byte,
	befores [][]byte,
	deletions []bool,
	err error,
) {
	nanos = make([]int64, len(mutations))
	logical = make([]int, len(mutations))
	keys = make([]string, len(mutations))
	jsons = make([][]byte, len(mutations))
	befores = make([][]byte, len(mutations))
	deletions = make([]bool, len(mutations))

	numWorkers := runtime.GOMAXPROCS(0)
	eg, errCtx := errgroup.WithContext(ctx)
	for worker := 0; worker < numWorkers; worker++ {
		worker := worker
		eg.Go(func() error {
			for idx := worker; idx < len(jsons); idx += numWorkers {
				if err := errCtx.Err(); err != nil {
					return err
				}
				var err error
				mut := mutations[idx]

				nanos[idx] = mut.Time.Nanos()
				logical[idx] = mut.Time.Logical()
				keys[idx] = string(mut.Key)
				deletions[idx] = mut.IsDelete()
				befores[idx], err = maybeGZip(mut.Before)
				if err != nil {
					return err
				}
				if mut.HasData() {
					jsons[idx], err = maybeGZip(mut.Data)
					if err != nil {
						return err
					}
				} else {
					jsons[idx] = []byte("null")
				}
			}
			return nil
		})
	}

	err = eg.Wait()
	return
}

// stageOneBatch appends the mutations to the staging table.
func (s *stage) stageOneBatch(
	ctx context.Context, db types.StagingQuerier, mutations []types.Mutation,
) error {
	nanos, logical, keys, jsons, befores, deletions, err := s.packArgs(ctx, mutations)
	if err != nil {
		return err
	}
	tag, err := db.Exec(ctx, s.sql.stage, nanos, logical, keys, jsons, befores, deletions)
	if err != nil {
		return errors.Wrap(err, s.sql.stage)
	}

	// Track re-delivered mutations. Some small number are normal since
	// changefeeds guarantee at-least-once delivery of data.
	if dupCount := int64(len(mutations)) - tag.RowsAffected(); dupCount > 0 {
		s.stageDupes.Add(float64(dupCount))
		log.Tracef("stage: received %d duplication mutations for %s", dupCount, s.stage)
	}
	return nil
}

const markAppliedTemplate = `
WITH t (key, nanos, logical) AS (SELECT unnest($1::STRING[]), unnest($2::INT8[]), unnest($3::INT8[]))
INSERT INTO %s (key, nanos, logical, applied, applied_at, mut)
SELECT t.key, t.nanos, t.logical, true, now(), '%s' FROM t
ON CONFLICT (key, nanos, logical)
DO UPDATE SET applied = true, applied_at = now()
`

// MarkApplied sets the applied column to true for the given mutations.
// If the mutation does not already exist, a stub entry will be created.
func (s *stage) MarkApplied(
	ctx context.Context, db types.StagingQuerier, muts []types.Mutation,
) error {
	keys := make([]json.RawMessage, len(muts))
	nanos := make([]int64, len(muts))
	logical := make([]int, len(muts))
	for idx, mut := range muts {
		keys[idx] = mut.Key
		nanos[idx] = mut.Time.Nanos()
		logical[idx] = mut.Time.Logical()
	}
	return retry.Retry(ctx, s.stagingDB, func(ctx context.Context) error {
		start := time.Now()

		// 1) number of mutations larger than the `markAppliedBatchSize`: we
		// want to introduce a transaction here to keep the updates for a
		// batch's apply times atomic. However, we only want to do this if there
		// is not already a transaction.
		//
		// 2) extraSanityChecks: we need a transaction in order to check if the
		// data is consistent.
		var tx pgx.Tx
		if extraSanityChecks || len(muts) > s.cfg.MarkAppliedLimit {
			if _, isTx := db.(pgx.Tx); !isTx {
				var err error
				tx, err = s.stagingDB.Begin(ctx)
				if err != nil {
					return errors.WithStack(err)
				}
				defer func() { _ = tx.Rollback(context.Background()) }()
				db = tx
			}
		}

		// Applies the mutations in batches to avoid exceeding the
		// `sql.conn.max_read_buffer_message_size` This also reduces
		// the memory being used during this step in the case there
		// are millions or more rows.
		if err := batches.Window(s.cfg.MarkAppliedLimit, len(muts), func(begin, end int) error {
			lenWindow := end - begin
			keys := make([]json.RawMessage, lenWindow)
			nanos := make([]int64, lenWindow)
			logical := make([]int, lenWindow)
			for idx, mut := range muts[begin:end] {
				keys[idx] = mut.Key
				nanos[idx] = mut.Time.Nanos()
				logical[idx] = mut.Time.Logical()
			}

			tag, err := db.Exec(ctx, s.sql.markApplied, keys, nanos, logical)
			if err != nil {
				return errors.Wrap(err, s.sql.markApplied)
			}

			log.Tracef("MarkApplied: %s marked %d mutations", s.stage, tag.RowsAffected())
			return nil
		}); err != nil {
			return err
		}

		if extraSanityChecks {
			count, err := s.CheckConsistency(ctx, db, muts, false /* current-time read */)
			if err != nil {
				return err
			}
			if count != 0 {
				return errors.Errorf("consistency check failed with %d mutations", count)
			}
		}
		s.markDuration.Observe(time.Since(start).Seconds())
		if tx != nil {
			return errors.WithStack(tx.Commit(ctx))
		}
		return nil
	})
}

const retireTemplate = `
WITH d AS (
     DELETE FROM %s
      WHERE (nanos, logical) BETWEEN ($1, $2) AND ($3, $4) AND applied
   ORDER BY nanos, logical
      LIMIT $5
  RETURNING nanos, logical)
SELECT last_value(nanos) OVER (), last_value(logical) OVER ()
  FROM d
 LIMIT 1`

// Retire deletes staged data up to the given end time.
func (s *stage) Retire(ctx context.Context, db types.StagingQuerier, end hlc.Time) error {
	start := time.Now()
	err := retry.Retry(ctx, s.stagingDB, func(ctx context.Context) error {
		from, _ := s.retireFrom.Get()
		for hlc.Compare(from, end) < 0 {
			var lastNanos int64
			var lastLogical int
			err := db.QueryRow(ctx, s.sql.retire,
				from.Nanos(),
				from.Logical(),
				end.Nanos(),
				end.Logical(),
				10000, // Make configurable?
			).Scan(&lastNanos, &lastLogical)

			if errors.Is(err, pgx.ErrNoRows) {
				break
			}
			if err != nil {
				return errors.WithStack(err)
			}
			from = hlc.New(lastNanos, lastLogical)
		}
		// If there was nothing to delete, still advance the marker.
		if hlc.Compare(from, end) < 0 {
			from = end
		}
		s.retireFrom.Set(from)
		return nil
	})
	if err == nil {
		s.retireDuration.Observe(time.Since(start).Seconds())
	} else {
		s.retireError.Inc()
	}
	return err
}
