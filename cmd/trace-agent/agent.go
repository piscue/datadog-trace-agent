package main

import (
	"context"
	"sync/atomic"
	"time"

	log "github.com/cihub/seelog"

	"github.com/DataDog/datadog-trace-agent/api"
	"github.com/DataDog/datadog-trace-agent/config"
	"github.com/DataDog/datadog-trace-agent/event"
	"github.com/DataDog/datadog-trace-agent/filters"
	"github.com/DataDog/datadog-trace-agent/info"
	"github.com/DataDog/datadog-trace-agent/model"
	"github.com/DataDog/datadog-trace-agent/obfuscate"
	"github.com/DataDog/datadog-trace-agent/osutil"
	"github.com/DataDog/datadog-trace-agent/sampler"
	"github.com/DataDog/datadog-trace-agent/statsd"
	"github.com/DataDog/datadog-trace-agent/watchdog"
	"github.com/DataDog/datadog-trace-agent/writer"
)

const processStatsInterval = time.Minute

// Agent struct holds all the sub-routines structs and make the data flow between them
type Agent struct {
	Receiver           *api.HTTPReceiver
	Concentrator       *Concentrator
	Blacklister        *filters.Blacklister
	Replacer           *filters.Replacer
	ScoreSampler       *Sampler
	ErrorsScoreSampler *Sampler
	PrioritySampler    *Sampler
	EventProcessor     *event.Processor
	TraceWriter        *writer.TraceWriter
	ServiceWriter      *writer.ServiceWriter
	StatsWriter        *writer.StatsWriter
	ServiceExtractor   *TraceServiceExtractor
	ServiceMapper      *ServiceMapper

	// obfuscator is used to obfuscate sensitive data from various span
	// tags based on their type.
	obfuscator *obfuscate.Obfuscator

	tracePkgChan chan *writer.TracePackage

	// config
	conf    *config.AgentConfig
	dynConf *config.DynamicConfig

	// Used to synchronize on a clean exit
	ctx context.Context
}

// NewAgent returns a new Agent object, ready to be started. It takes a context
// which may be cancelled in order to gracefully stop the agent.
func NewAgent(ctx context.Context, conf *config.AgentConfig) *Agent {
	dynConf := config.NewDynamicConfig()

	// inter-component channels
	rawTraceChan := make(chan model.Trace, 5000) // about 1000 traces/sec for 5 sec, TODO: move to *model.Trace
	tracePkgChan := make(chan *writer.TracePackage)
	statsChan := make(chan []model.StatsBucket)
	serviceChan := make(chan model.ServicesMetadata, 50)
	filteredServiceChan := make(chan model.ServicesMetadata, 50)

	// create components
	r := api.NewHTTPReceiver(conf, dynConf, rawTraceChan, serviceChan)
	c := NewConcentrator(
		conf.ExtraAggregators,
		conf.BucketInterval.Nanoseconds(),
		statsChan,
	)

	obf := obfuscate.NewObfuscator(conf.Obfuscation)
	ss := NewScoreSampler(conf)
	ess := NewErrorsSampler(conf)
	ps := NewPrioritySampler(conf, dynConf)
	ep := eventProcessorFromConf(conf)
	se := NewTraceServiceExtractor(serviceChan)
	sm := NewServiceMapper(serviceChan, filteredServiceChan)
	tw := writer.NewTraceWriter(conf, tracePkgChan)
	sw := writer.NewStatsWriter(conf, statsChan)
	svcW := writer.NewServiceWriter(conf, filteredServiceChan)

	return &Agent{
		Receiver:           r,
		Concentrator:       c,
		Blacklister:        filters.NewBlacklister(conf.Ignore["resource"]),
		Replacer:           filters.NewReplacer(conf.ReplaceTags),
		ScoreSampler:       ss,
		ErrorsScoreSampler: ess,
		PrioritySampler:    ps,
		EventProcessor:     ep,
		TraceWriter:        tw,
		StatsWriter:        sw,
		ServiceWriter:      svcW,
		ServiceExtractor:   se,
		ServiceMapper:      sm,
		obfuscator:         obf,
		tracePkgChan:       tracePkgChan,
		conf:               conf,
		dynConf:            dynConf,
		ctx:                ctx,
	}
}

// Run starts routers routines and individual pieces then stop them when the exit order is received
func (a *Agent) Run() {
	// it's really important to use a ticker for this, and with a not too short
	// interval, for this is our guarantee that the process won't start and kill
	// itself too fast (nightmare loop)
	watchdogTicker := time.NewTicker(a.conf.WatchdogInterval)
	defer watchdogTicker.Stop()

	// update the data served by expvar so that we don't expose a 0 sample rate
	info.UpdatePreSampler(*a.Receiver.PreSampler.Stats())

	// TODO: unify components APIs. Use Start/Stop as non-blocking ways of controlling the blocking Run loop.
	// Like we do with TraceWriter.
	a.Receiver.Run()
	a.TraceWriter.Start()
	a.StatsWriter.Start()
	a.ServiceMapper.Start()
	a.ServiceWriter.Start()
	a.Concentrator.Start()
	a.ScoreSampler.Run()
	a.ErrorsScoreSampler.Run()
	a.PrioritySampler.Run()
	a.EventProcessor.Start()

	for {
		select {
		case t := <-a.Receiver.Out:
			a.Process(t)
		case <-watchdogTicker.C:
			a.watchdog()
		case <-a.ctx.Done():
			log.Info("exiting")
			if err := a.Receiver.Stop(); err != nil {
				log.Error(err)
			}
			a.Concentrator.Stop()
			a.TraceWriter.Stop()
			a.StatsWriter.Stop()
			a.ServiceMapper.Stop()
			a.ServiceWriter.Stop()
			a.ScoreSampler.Stop()
			a.ErrorsScoreSampler.Stop()
			a.PrioritySampler.Stop()
			a.EventProcessor.Stop()
			return
		}
	}
}

// Process is the default work unit that receives a trace, transforms it and
// passes it downstream.
func (a *Agent) Process(t model.Trace) {
	if len(t) == 0 {
		log.Debugf("skipping received empty trace")
		return
	}

	// Root span is used to carry some trace-level metadata, such as sampling rate and priority.
	root := t.GetRoot()

	// We get the address of the struct holding the stats associated to no tags.
	// TODO: get the real tagStats related to this trace payload (per lang/version).
	ts := a.Receiver.Stats.GetTagStats(info.Tags{})

	// Extract priority early, as later goroutines might manipulate the Metrics map in parallel which isn't safe.
	priority, hasPriority := root.GetSamplingPriority()

	// Depending on the sampling priority, count that trace differently.
	stat := &ts.TracesPriorityNone
	if hasPriority {
		if priority < 0 {
			stat = &ts.TracesPriorityNeg
		} else if priority == 0 {
			stat = &ts.TracesPriority0
		} else if priority == 1 {
			stat = &ts.TracesPriority1
		} else {
			stat = &ts.TracesPriority2
		}
	}
	atomic.AddInt64(stat, 1)

	if !a.Blacklister.Allows(root) {
		log.Debugf("trace rejected by blacklister. root: %v", root)
		atomic.AddInt64(&ts.TracesFiltered, 1)
		atomic.AddInt64(&ts.SpansFiltered, int64(len(t)))
		return
	}

	// Extra sanitization steps of the trace.
	for _, span := range t {
		a.obfuscator.Obfuscate(span)
		span.Truncate()
	}
	a.Replacer.Replace(&t)

	// Extract the client sampling rate.
	clientSampleRate := root.GetSampleRate()
	root.SetClientTraceSampleRate(clientSampleRate)
	// Combine it with the pre-sampling rate.
	preSamplerRate := a.Receiver.PreSampler.Rate()
	root.SetPreSampleRate(preSamplerRate)
	// Update root's global sample rate to include the presampler rate as well
	root.UpdateSampleRate(preSamplerRate)

	// Figure out the top-level spans and sublayers now as it involves modifying the Metrics map
	// which is not thread-safe while samplers and Concentrator might modify it too.
	t.ComputeTopLevel()

	subtraces := t.ExtractTopLevelSubtraces(root)
	sublayers := make(map[*model.Span][]model.SublayerValue)
	for _, subtrace := range subtraces {
		subtraceSublayers := model.ComputeSublayers(subtrace.Trace)
		sublayers[subtrace.Root] = subtraceSublayers
		model.SetSublayersOnSpan(subtrace.Root, subtraceSublayers)
	}

	pt := model.ProcessedTrace{
		Trace:         t,
		WeightedTrace: model.NewWeightedTrace(t, root),
		Root:          root,
		Env:           a.conf.DefaultEnv,
		Sublayers:     sublayers,
	}
	// Replace Agent-configured environment with `env` coming from span tag.
	if tenv := t.GetEnv(); tenv != "" {
		pt.Env = tenv
	}

	go func() {
		defer watchdog.LogOnPanic()
		a.ServiceExtractor.Process(pt.WeightedTrace)
	}()

	go func(pt model.ProcessedTrace) {
		defer watchdog.LogOnPanic()
		// Everything is sent to concentrator for stats, regardless of sampling.
		a.Concentrator.Add(pt)
	}(pt)

	// Don't go through sampling for < 0 priority traces
	if priority < 0 {
		return
	}
	// Run both full trace sampling and transaction extraction in another goroutine.
	go func(pt model.ProcessedTrace) {
		defer watchdog.LogOnPanic()

		tracePkg := writer.TracePackage{}

		sampled, rate := a.sample(pt)

		if sampled {
			pt.Sampled = sampled
			pt.Root.UpdateSampleRate(rate)
			tracePkg.Trace = pt.Trace
		}

		// NOTE: Events can be processed on non-sampled traces.
		events, numExtracted := a.EventProcessor.Process(pt)
		tracePkg.Events = events

		atomic.AddInt64(&ts.EventsExtracted, int64(numExtracted))
		atomic.AddInt64(&ts.EventsSampled, int64(len(tracePkg.Events)))

		if !tracePkg.Empty() {
			a.tracePkgChan <- &tracePkg
		}
	}(pt)
}

func (a *Agent) sample(pt model.ProcessedTrace) (sampled bool, rate float64) {
	var sampledPriority, sampledScore bool
	var ratePriority, rateScore float64

	if _, ok := pt.GetSamplingPriority(); ok {
		sampledPriority, ratePriority = a.PrioritySampler.Add(pt)
	}

	if traceContainsError(pt.Trace) {
		sampledScore, rateScore = a.ErrorsScoreSampler.Add(pt)
	} else {
		sampledScore, rateScore = a.ScoreSampler.Add(pt)
	}

	return sampledScore || sampledPriority, sampler.CombineRates(ratePriority, rateScore)
}

// dieFunc is used by watchdog to kill the agent; replaced in tests.
var dieFunc = func(fmt string, args ...interface{}) {
	osutil.Exitf(fmt, args...)
}

func (a *Agent) watchdog() {
	var wi watchdog.Info
	wi.CPU = watchdog.CPU()
	wi.Mem = watchdog.Mem()
	wi.Net = watchdog.Net()

	if float64(wi.Mem.Alloc) > a.conf.MaxMemory && a.conf.MaxMemory > 0 {
		dieFunc("exceeded max memory (current=%d, max=%d)", wi.Mem.Alloc, int64(a.conf.MaxMemory))
	}
	if int(wi.Net.Connections) > a.conf.MaxConnections && a.conf.MaxConnections > 0 {
		dieFunc("exceeded max connections (current=%d, max=%d)", wi.Net.Connections, a.conf.MaxConnections)
	}

	info.UpdateWatchdogInfo(wi)

	// Adjust pre-sampling dynamically
	rate, err := sampler.CalcPreSampleRate(a.conf.MaxCPU, wi.CPU.UserAvg, a.Receiver.PreSampler.RealRate())
	if err != nil {
		log.Warnf("problem computing pre-sample rate: %v", err)
	}
	a.Receiver.PreSampler.SetRate(rate)
	a.Receiver.PreSampler.SetError(err)

	preSamplerStats := a.Receiver.PreSampler.Stats()
	statsd.Client.Gauge("datadog.trace_agent.presampler_rate", preSamplerStats.Rate, nil, 1)
	info.UpdatePreSampler(*preSamplerStats)
}

func traceContainsError(trace model.Trace) bool {
	for _, span := range trace {
		if span.Error != 0 {
			return true
		}
	}
	return false
}

func eventProcessorFromConf(conf *config.AgentConfig) *event.Processor {
	extractors := []event.Extractor{
		event.NewMetricBasedExtractor(),
	}
	if len(conf.AnalyzedSpansByService) > 0 {
		extractors = append(extractors, event.NewFixedRateExtractor(conf.AnalyzedSpansByService))
	} else if len(conf.AnalyzedRateByServiceLegacy) > 0 {
		extractors = append(extractors, event.NewLegacyExtractor(conf.AnalyzedRateByServiceLegacy))
	}

	return event.NewProcessor(extractors, event.NewMaxEPSSampler(conf.MaxEPS))
}
