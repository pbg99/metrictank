package defcache

import (
	"github.com/grafana/grafana/pkg/log"
	"github.com/raintank/met"
	"github.com/raintank/raintank-metric/metric_tank/idx"
	"github.com/raintank/raintank-metric/metricdef"
	"github.com/raintank/raintank-metric/schema"
	"sync"
	"time"
)

var (
	metricsToEsOK           met.Count
	metricsToEsFail         met.Count
	esPutDuration           met.Timer // note that due to our use of bulk indexer, most values will be very fast with the occasional "outlier" which triggers a flush
	idxPruneDuration        met.Timer
	idxGetDuration          met.Timer
	idxListDuration         met.Timer
	idxMatchLiteralDuration met.Timer
	idxMatchPrefixDuration  met.Timer
	idxMatchTrigramDuration met.Timer
)

// design notes:
// MT pulls in all definitions when it starts up.
// those "old" ones + whatever it sees as inputs from the metrics queue
// is enough for it to always know the complete current state
// nothing should update ES "behind its back", so we never need to pull
// from ES other then at startup.
// but other MT instances may update ES while we are down, so ES is a good
// place to pull from, until the performance is demonstrably too slow.
// there are some vectors here for race conditions but we can work those out
// later, perhaps when tacking the multiple-intervals work

type DefCache struct {
	sync.RWMutex
	defs      []schema.MetricDefinition
	ById      map[string]idx.MetricID // by hashed id. we store uints, not pointers, to lower GC workload.
	ByKey     *idx.Idx                // by graphite key aka "Name" in the def to support graphite native api. this index is experimental and may be removed in the future
	defsStore metricdef.Defs
}

func New(defsStore metricdef.Defs, stats met.Backend) *DefCache {
	metricsToEsOK = stats.NewCount("metrics_to_es.ok")
	metricsToEsFail = stats.NewCount("metrics_to_es.fail")
	esPutDuration = stats.NewTimer("es_put_duration", 0)
	idxPruneDuration = stats.NewTimer("idx.prune_duration", 0)
	idxGetDuration = stats.NewTimer("idx.get_duration", 0)
	idxListDuration = stats.NewTimer("idx.list_duration", 0)
	idxMatchLiteralDuration = stats.NewTimer("idx.match_literal_duration", 0)
	idxMatchPrefixDuration = stats.NewTimer("idx.match_prefix_duration", 0)
	idxMatchTrigramDuration = stats.NewTimer("idx.match_trigram_duration", 0)

	d := &DefCache{
		ById:      make(map[string]idx.MetricID),
		ByKey:     idx.New(),
		defsStore: defsStore,
	}
	go d.Prune()
	d.Backfill()
	return d
}

func (dc *DefCache) Prune() {
	t := time.Tick(3 * time.Minute)
	for range t {
		// there's some fragments that occur in a whole lot of metrics
		// for example 'litmus'
		// this only retains the trigram postlists in the index if <20%
		// of the metrics contain them.  this keeps memory usage down
		// and makes queries faster
		pre := time.Now()
		dc.Lock()
		dc.ByKey.Prune(0.20)
		dc.Unlock()
		idxPruneDuration.Value(time.Now().Sub(pre))
	}
}

// backfill definitions from ES
// in theory, there is a race between defs from ES and from nsq
// in practice, it doesn't matter: you're only supposed to query MT
// after a while, after which the defs surely have stabilized.
func (dc *DefCache) Backfill() {
	total := 0
	add := func(met []*schema.MetricDefinition) {
		if len(met) > 0 {
			total += len(met)
			dc.Lock()
			for _, def := range met {
				id := dc.ByKey.GetOrAdd(def.OrgId, def.Name) // gets id auto assigned from 0 and onwards
				dc.ByKey.AddRef(def.OrgId, id)
				dc.ById[def.Id] = id
				dc.defs = append(dc.defs, *def) // which maps 1:1 with pos in this array
			}
			dc.Unlock()
		}
	}
	met, scroll_id, err := dc.defsStore.GetMetrics("")
	if err != nil {
		log.Error(3, "Could not backfill from ES: %s", err)
		return
	}
	add(met)
	for scroll_id != "" {
		met, scroll_id, err = dc.defsStore.GetMetrics(scroll_id)
		if err != nil {
			log.Error(3, "Could not backfill from ES: %s", err)
			return
		}
		add(met)
	}
	log.Debug("backfilled %d metric definitions", total)
}

// Adds the metric to the defcache.
// after this function returns, it is safe to modify the data pointed to
func (dc *DefCache) Add(metric *schema.MetricData) {
	dc.RLock()
	id, ok := dc.ById[metric.Id]
	dc.RUnlock()
	if ok {
		//If the time diff between this datapoint and the lastUpdate
		// time of the metricDef is greater than 6hours, update the metricDef.
		dc.RLock()
		mdef := dc.defs[id]
		dc.RUnlock()
		if mdef.LastUpdate < metric.Time-21600 {
			// this is a little expensive, let's not hold the lock while we do this
			mdef = *schema.MetricDefinitionFromMetricData(metric)
			// let's make sure only one concurrent Add() can addToES,
			// because that function is a bit expensive and could block
			// so now that we have the mdef, let's check again before proceeding.
			dc.Lock()
			old := dc.defs[id]
			if old.LastUpdate < metric.Time-21600 {
				dc.addToES(&mdef)
				dc.defs[id] = mdef
			}
			dc.Unlock()
		}
	} else {
		mdef := *schema.MetricDefinitionFromMetricData(metric)
		// now that we have the mdef, let's make sure we only add this once concurrently.
		// because addToES is pretty expensive and we should only call AddRef once.
		dc.Lock()
		id, ok := dc.ById[metric.Id]
		if ok {
			// someone beat us to it. nothing left to do
			dc.Unlock()
			return
		}
		dc.addToES(&mdef)
		id = dc.ByKey.GetOrAdd(mdef.OrgId, mdef.Name)
		dc.ByKey.AddRef(mdef.OrgId, id)
		dc.ById[mdef.Id] = id
		dc.defs = append(dc.defs, mdef)
		dc.Unlock()
	}
}

func (dc *DefCache) addToES(mdef *schema.MetricDefinition) {
	pre := time.Now()
	err := dc.defsStore.IndexMetric(mdef)
	// NOTE: indexing to ES is done asyncrounously using the bulkAPI.
	// so an error here is just an error adding the document to the
	// bulkAPI buffer.
	if err != nil {
		log.Error(3, "couldn't index to ES %s: %s", mdef.Id, err)
		metricsToEsFail.Inc(1)
	} else {
		metricsToEsOK.Inc(1)
	}
	esPutDuration.Value(time.Now().Sub(pre))
}

// Get gets a metricdef by id
// note: the defcache is clearly not a perfect all-knowning entity, it just knows the last interval of metrics seen since program start
// and we assume we can use that interval through history.
// TODO: no support for interval changes, missing datablocks, ...
// note: do *not* modify the pointed-to data, as it will affect the data in the index!
func (dc *DefCache) Get(id string) (*schema.MetricDefinition, bool) {
	var def *schema.MetricDefinition
	pre := time.Now()
	dc.RLock()
	i, ok := dc.ById[id]
	if ok {
		def = &dc.defs[i]
	}
	dc.RUnlock()
	idxGetDuration.Value(time.Now().Sub(pre))
	return def, ok
}

// Find returns the results of a pattern match.
// note: do *not* modify the pointed-to data, as it will affect the data in the index!
func (dc *DefCache) Find(org int, key string) ([]idx.Glob, []*schema.MetricDefinition) {
	pre := time.Now()
	dc.RLock()
	mt, globs := dc.ByKey.Match(org, key)
	defs := make([]*schema.MetricDefinition, len(globs))
	for i, g := range globs {
		defs[i] = &dc.defs[g.Id]
	}
	dc.RUnlock()
	switch mt {
	case idx.MatchLiteral:
		idxMatchLiteralDuration.Value(time.Now().Sub(pre))
	case idx.MatchPrefix:
		idxMatchPrefixDuration.Value(time.Now().Sub(pre))
	case idx.MatchTrigram:
		idxMatchTrigramDuration.Value(time.Now().Sub(pre))
	}
	return globs, defs
}

// List provides all metricdefs based on the provide org. if it is:
// -1, then all metricdefs for all orgs are returned
// any other org, then all defs for that org, as well as -1 are returned
// note: do *not* modify the pointed-to data, as it will affect the data in the index!
func (dc *DefCache) List(org int) []*schema.MetricDefinition {
	pre := time.Now()
	dc.RLock()
	list := dc.ByKey.List(org)
	out := make([]*schema.MetricDefinition, len(list))
	for i, id := range list {
		out[i] = &dc.defs[id]
	}
	dc.RUnlock()
	idxListDuration.Value(time.Now().Sub(pre))
	return out
}
