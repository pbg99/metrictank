package main

import (
	"fmt"
	"github.com/raintank/raintank-metric/metric_tank/consolidation"
)

type Req struct {
	// these fields can be set straight away:
	key          string
	from         uint32
	to           uint32
	minPoints    uint32
	maxPoints    uint32
	consolidator consolidation.Consolidator

	// these fields need some more coordination and are typically set later
	archive      int    // -1 means original data, 0 last agg level, 1 2nd last, etc.
	rawInterval  uint32 // the interval of the raw metric before any consolidation
	archInterval uint32 // the interval we want for the fetch, should match the archive interval
	outInterval  uint32 // the interval of the output data, after any runtime consolidation
	aggNum       uint32 // how many points to consolidate together at runtime, from the archive
}

func NewReq(key string, from, to, minPoints, maxPoints uint32, consolidator consolidation.Consolidator) Req {
	return Req{
		key,
		from,
		to,
		minPoints,
		maxPoints,
		consolidator,
		-2, // this is supposed to be updated still!
		0,  // this is supposed to be updated still
		0,  // this is supposed to be updated still
		0,  // this is supposed to be updated still
		0,  // this is supposed to be updated still
	}
}

func (r Req) String() string {
	return fmt.Sprintf("%s %d - %d (%s - %s) span:%ds. %d <= points <= %d. %s", r.key, r.from, r.to, TS(r.from), TS(r.to), r.to-r.from-1, r.minPoints, r.maxPoints, r.consolidator)
}

func (r Req) DebugString() string {
	return fmt.Sprintf("%s %d - %d . %d <= points <= %d. %s - archive %d, rawInt %d, archInt %d, outInt %d, aggNum %d",
		r.key, r.from, r.to, r.minPoints, r.maxPoints, r.consolidator, r.archive, r.rawInterval, r.archInterval, r.outInterval, r.aggNum)
}
