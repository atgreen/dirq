// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package agent

import (
	"sync"
	"time"

	pb "github.com/atgreen/dirq/proto/dirq/v1"
)

// queryAggregator buffers QueryResults from downstream peers per query_id.
// After an idle timeout (no new results for a period), it flushes the buffer
// as a single AggregatedQueryResult upstream. This reduces message count
// from O(agents) to O(zone_leaders) at the server.
type queryAggregator struct {
	mu       sync.Mutex
	buffers  map[string]*queryBuffer
	flushFn  func(*pb.AggregatedQueryResult) // called when a buffer flushes
	idleTime time.Duration                   // flush after this long with no new results
}

type queryBuffer struct {
	queryID  string
	results  []*pb.QueryResult
	timer    *time.Timer
	flushed  bool
}

func newQueryAggregator(idleTime time.Duration, flushFn func(*pb.AggregatedQueryResult)) *queryAggregator {
	return &queryAggregator{
		buffers:  make(map[string]*queryBuffer),
		flushFn:  flushFn,
		idleTime: idleTime,
	}
}

// add buffers a single QueryResult. Resets the idle timer each time.
func (qa *queryAggregator) add(result *pb.QueryResult) {
	qa.mu.Lock()
	defer qa.mu.Unlock()

	queryID := result.QueryId
	buf, ok := qa.buffers[queryID]
	if !ok {
		buf = &queryBuffer{queryID: queryID}
		buf.timer = time.AfterFunc(qa.idleTime, func() {
			qa.flush(queryID)
		})
		qa.buffers[queryID] = buf
	}

	if buf.flushed {
		// Late arrival after flush — send individually via a new single-result aggregate.
		qa.flushFn(&pb.AggregatedQueryResult{
			QueryId: queryID,
			Results: []*pb.QueryResult{result},
		})
		return
	}

	buf.results = append(buf.results, result)

	// Reset idle timer — more results may be coming.
	buf.timer.Reset(qa.idleTime)
}

// addAggregated merges an already-aggregated batch into the buffer.
// This happens when a downstream relay flushes its subtree to us.
func (qa *queryAggregator) addAggregated(agg *pb.AggregatedQueryResult) {
	qa.mu.Lock()
	defer qa.mu.Unlock()

	queryID := agg.QueryId
	buf, ok := qa.buffers[queryID]
	if !ok {
		buf = &queryBuffer{queryID: queryID}
		buf.timer = time.AfterFunc(qa.idleTime, func() {
			qa.flush(queryID)
		})
		qa.buffers[queryID] = buf
	}

	if buf.flushed {
		// Late arrival — forward as-is.
		qa.flushFn(agg)
		return
	}

	buf.results = append(buf.results, agg.Results...)
	buf.timer.Reset(qa.idleTime)
}

// flush sends all buffered results for a query as one aggregated message.
func (qa *queryAggregator) flush(queryID string) {
	qa.mu.Lock()
	buf, ok := qa.buffers[queryID]
	if !ok || buf.flushed {
		qa.mu.Unlock()
		return
	}
	buf.flushed = true
	buf.timer.Stop()
	results := buf.results
	buf.results = nil
	// Clean up the buffer after a grace period for late arrivals.
	time.AfterFunc(30*time.Second, func() {
		qa.mu.Lock()
		delete(qa.buffers, queryID)
		qa.mu.Unlock()
	})
	qa.mu.Unlock()

	if len(results) > 0 {
		qa.flushFn(&pb.AggregatedQueryResult{
			QueryId: queryID,
			Results: results,
		})
	}
}
