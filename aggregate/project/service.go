package project

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/modernice/goes/event"
	"github.com/modernice/goes/event/query"
	"github.com/modernice/goes/event/query/version"
)

// Context is the context for projecting Aggregates.
type Context interface {
	context.Context

	// AggregateName returns the name of the Aggregate that is being projected.
	AggregateName() string

	// AggregateID returns the UUID of the Aggregate that is being projected.
	AggregateID() uuid.UUID

	// Project runs the projection on the provided Projection.
	Project(context.Context, Projection) error
}

type pcontext struct {
	context.Context
	Projector

	aggregateName string
	aggregateID   uuid.UUID
}

// A Schedule decides when to run projections.
type Schedule interface {
	jobs(context.Context) (<-chan job, <-chan error, error)
}

// SubscribeOption is an option for Subscribe.
type SubscribeOption func(*subscription)

type subscription struct {
	stopTimeout time.Duration
	queryOpts   []query.Option
	query       query.Query

	proj     Projector
	schedule Schedule

	out        chan Context
	errs       chan error
	stop       chan struct{}
	handleDone chan struct{}
}

// ScheduleOption is an option for creating Schedules.
type ScheduleOption func(*scheduleConfig)

type scheduleConfig struct {
	filter []query.Option
}

type continously struct {
	filter query.Query
	bus    event.Bus
	events []string
}

type periodically struct {
	filter   query.Query
	store    event.Store
	interval time.Duration
	names    []string
}

type job struct {
	aggregateName string
	aggregateID   uuid.UUID
}

// Continuously returns a Schedule that instructs to project Aggregates on every
// change.
func Continuously(bus event.Bus, events []string, opts ...ScheduleOption) Schedule {
	cfg := newScheduleConfig(opts...)
	c := &continously{
		filter: query.New(cfg.filter...),
		bus:    bus,
		events: events,
	}
	return c
}

func newScheduleConfig(opts ...ScheduleOption) scheduleConfig {
	var cfg scheduleConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// Periodically returns a Schedule that instructs to project Aggregates
// periodically every Duration d. If no Aggregate names are provided, every
// Aggregate will be projected; otherwise only Aggregates with one of the
// provided names will be projected.
func Periodically(store event.Store, d time.Duration, aggregates []string, opts ...ScheduleOption) Schedule {
	cfg := newScheduleConfig(opts...)
	return &periodically{
		filter:   query.New(cfg.filter...),
		store:    store,
		interval: d,
		names:    aggregates,
	}
}

// StopTimeout returns a SubscribeOption that defines the timeout for projecting
// remaining Aggregates after a subscription has been canceled.
func StopTimeout(d time.Duration) SubscribeOption {
	return func(s *subscription) {
		s.stopTimeout = d
	}
}

// FilterEvents returns a ScheduleOption that filters Events of Aggregates when
// determining if an Event should trigger a projection. Events that match the
// Query won't trigger a projection.
func FilterEvents(opts ...query.Option) ScheduleOption {
	return func(cfg *scheduleConfig) {
		cfg.filter = append(cfg.filter, opts...)
	}
}

// Subscribe subscribes to the Schedule s and returns a channel of Contexts and
// an error channel.
func Subscribe(
	ctx context.Context,
	s Schedule,
	proj Projector,
	opts ...SubscribeOption,
) (<-chan Context, <-chan error, error) {
	sub := newSubscription(proj, s, opts...)

	jobs, errs, err := s.jobs(ctx)
	if err != nil {
		return nil, nil, err
	}

	go sub.handleJobs(jobs)
	go sub.handleCancel(ctx)

	return sub.out, errs, nil
}

func (sub *subscription) handleJobs(jobs <-chan job) {
	defer close(sub.handleDone)
	defer close(sub.out)
	for {
		select {
		case <-sub.stop:
			return
		case job, ok := <-jobs:
			if !ok {
				return
			}
			select {
			case <-sub.stop:
				return
			case sub.out <- newContext(
				context.Background(),
				sub.proj,
				job.aggregateName,
				job.aggregateID,
			):
			}
		}
	}
}

func (sub *subscription) handleCancel(parent context.Context) {
	defer close(sub.stop)

	select {
	case <-sub.handleDone:
		return
	case <-parent.Done():
	}

	var timeout <-chan time.Time
	if sub.stopTimeout > 0 {
		timer := time.NewTimer(sub.stopTimeout)
		defer timer.Stop()
		timeout = timer.C
	}

	select {
	case <-sub.handleDone:
	case <-timeout:
	}
}

func (c *continously) jobs(ctx context.Context) (<-chan job, <-chan error, error) {
	events, errs, err := c.bus.Subscribe(ctx, c.events...)
	if err != nil {
		return nil, nil, fmt.Errorf("subscribe to %v events: %w", c.events, err)
	}

	jobs := make(chan job)
	go c.handleEvents(events, jobs)

	return jobs, errs, nil
}

func (c *continously) handleEvents(events <-chan event.Event, jobs chan<- job) {
	defer close(jobs)
	for evt := range events {
		if shouldDiscard(evt, c.filter) {
			continue
		}

		jobs <- job{
			aggregateName: evt.AggregateName(),
			aggregateID:   evt.AggregateID(),
		}
	}
}

func (p *periodically) jobs(ctx context.Context) (<-chan job, <-chan error, error) {
	ticker := time.NewTicker(p.interval)
	go func() {
		<-ctx.Done()
		ticker.Stop()
	}()
	jobs := make(chan job)
	errs := make(chan error)
	go p.handleTicks(ctx, ticker.C, jobs, errs)
	return jobs, errs, nil
}

func (p *periodically) handleTicks(
	ctx context.Context,
	ticker <-chan time.Time,
	jobs chan<- job,
	errs chan<- error,
) {
	defer close(jobs)
	defer close(errs)
	for range ticker {
		str, serrs, err := p.store.Query(ctx, query.New(
			query.AggregateName(p.names...),
			query.AggregateVersion(version.Exact(0)),
		))
		if err != nil {
			errs <- fmt.Errorf("query base events: %w", err)
			return
		}

		if err = event.Walk(ctx, func(evt event.Event) {
			if shouldDiscard(evt, p.filter) {
				return
			}

			jobs <- job{
				aggregateName: evt.AggregateName(),
				aggregateID:   evt.AggregateID(),
			}
		}, str, serrs); err != nil {
			errs <- fmt.Errorf("event stream: %w", err)
		}
	}
}

func (j *pcontext) AggregateName() string {
	return j.aggregateName
}

func (j *pcontext) AggregateID() uuid.UUID {
	return j.aggregateID
}

func newSubscription(proj Projector, s Schedule, opts ...SubscribeOption) *subscription {
	sub := subscription{
		proj:       proj,
		schedule:   s,
		out:        make(chan Context),
		errs:       make(chan error),
		stop:       make(chan struct{}),
		handleDone: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(&sub)
	}
	sub.query = query.New(sub.queryOpts...)
	return &sub
}

func newContext(ctx context.Context, proj Projector, name string, id uuid.UUID) *pcontext {
	return &pcontext{
		Context:       ctx,
		Projector:     proj,
		aggregateName: name,
		aggregateID:   id,
	}
}

func shouldDiscard(evt event.Event, q query.Query) bool {
	if evt.AggregateName() == "" || evt.AggregateID() == uuid.Nil {
		return true
	}
	return !query.Test(q, evt)
}
