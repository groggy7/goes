// Package mongostore provides a MongoDB event.Store.
package mongostore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	stdtime "time"

	"github.com/google/uuid"
	"github.com/modernice/goes/event"
	"github.com/modernice/goes/event/query/time"
	"github.com/modernice/goes/event/query/version"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Store is the MongoDB event.Store.
type Store struct {
	enc        event.Encoder
	dbname     string
	entriesCol string
	statesCol  string

	client  *mongo.Client
	db      *mongo.Database
	entries *mongo.Collection
	states  *mongo.Collection

	onceConnect sync.Once
}

// Option is a Store option.
type Option func(*Store)

// A VersionError means the insertion of Events failed because at least one of
// the Events has an invalid/inconsistent version.
type VersionError struct {
	// AggregateName is the name of the Aggregate.
	AggregateName string

	// AggregateID is the UUID of the Aggregate.
	AggregateID uuid.UUID

	// CurrentVersion is the current version of the Aggregate.
	CurrentVersion int

	// Event is the event with the invalid version.
	Event event.Event
}

type state struct {
	AggregateName string    `bson:"aggregateName"`
	AggregageID   uuid.UUID `bson:"aggregateId"`
	Version       int       `bson:"version"`
}

type entry struct {
	ID               uuid.UUID    `bson:"id"`
	Name             string       `bson:"name"`
	Time             stdtime.Time `bson:"time"`
	TimeNano         int64        `bson:"timeNano"`
	AggregateName    string       `bson:"aggregateName"`
	AggregateID      uuid.UUID    `bson:"aggregateId"`
	AggregateVersion int          `bson:"aggregateVersion"`
	Data             []byte       `bson:"data"`
}

type stream struct {
	enc event.Encoder
	cur *mongo.Cursor
	evt event.Event
	err error
}

// Client returns an Option that specifies the underlying mongo.Client to be
// used by the Store.
func Client(c *mongo.Client) Option {
	return func(s *Store) {
		s.client = c
	}
}

// Database returns an Option that sets the mongo database to use for the events.
func Database(name string) Option {
	return func(s *Store) {
		s.dbname = name
	}
}

// Collection returns an Option that sets the mongo collection where the Events
// are stored in.
func Collection(name string) Option {
	return func(s *Store) {
		s.entriesCol = name
	}
}

// StateCollection returns an Option that specifies the name of the Collection
// where the current state of Aggregates are stored in.
func StateCollection(name string) Option {
	return func(s *Store) {
		s.statesCol = name
	}
}

// New returns a MongoDB event.Store.
func New(enc event.Encoder, opts ...Option) *Store {
	s := Store{enc: enc}
	for _, opt := range opts {
		opt(&s)
	}
	if strings.TrimSpace(s.dbname) == "" {
		s.dbname = "event"
	}
	if strings.TrimSpace(s.entriesCol) == "" {
		s.entriesCol = "events"
	}
	if strings.TrimSpace(s.statesCol) == "" {
		s.statesCol = "states"
	}
	return &s
}

// Client returns the underlying mongo.Client. If no mongo.Client is provided
// with the Client option, Client returns nil until the connection to MongoDB
// has been established by either explicitly calling s.Connect or implicitly by
// calling s.Insert, s.Find, s.Delete or s.Query. Otherwise Client returns the
// provided mongo.Client.
func (s *Store) Client() *mongo.Client {
	return s.client
}

// Database returns the underlying mongo.Database. Database returns nil until
// the connection to MongoDB has been established by either explicitly calling
// s.Connect or implicitly by calling s.Insert, s.Find, s.Delete or s.Query.
func (s *Store) Database() *mongo.Database {
	return s.db
}

// Collection returns the underlying *mongo.Collection where the Events are
// stored in. Connection returns nil until the connection to MongoDB has been
// established by either explicitly calling s.Connect or implicitly by calling
// s.Insert, s.Find, s.Delete or s.Query.
func (s *Store) Collection() *mongo.Collection {
	return s.entries
}

// StateCollection returns the underlying *mongo.Collection where Aggregate
// states are stored in. Connection returns nil until the connection to MongoDB
// has been established by either explicitly calling s.Connect or implicitly by
// calling s.Insert, s.Find, s.Delete or s.Query.
func (s *Store) StateCollection() *mongo.Collection {
	return s.states
}

// Insert saves the given Events into the database.
func (s *Store) Insert(ctx context.Context, events ...event.Event) error {
	if err := s.connectOnce(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	return s.client.UseSession(ctx, func(ctx mongo.SessionContext) error {
		if err := ctx.StartTransaction(); err != nil {
			return fmt.Errorf("start transaction: %w", err)
		}

		st, err := s.validateVersion(ctx, events)
		if err != nil {
			return fmt.Errorf("validate version: %w", err)
		}

		for _, evt := range events {
			if err := s.insert(ctx, evt); err != nil {
				if err = ctx.AbortTransaction(ctx); err != nil {
					return fmt.Errorf("abort transaction: %w", err)
				}
				return fmt.Errorf("insert %s:%s: %w", evt.Name(), evt.ID(), err)
			}
		}

		if err := s.updateState(ctx, st, events); err != nil {
			if err = ctx.AbortTransaction(ctx); err != nil {
				return fmt.Errorf("abort transaction: %w", err)
			}
			return fmt.Errorf("update state: %w", err)
		}

		if err := ctx.CommitTransaction(ctx); err != nil {
			return fmt.Errorf("commit transaction: %w", err)
		}

		return nil
	})
}

func (s *Store) validateVersion(ctx mongo.SessionContext, events []event.Event) (state, error) {
	if len(events) == 0 {
		return state{}, nil
	}

	aggregateName := events[0].AggregateName()
	aggregateID := events[0].AggregateID()

	if aggregateName == "" || aggregateID == uuid.Nil {
		return state{}, nil
	}

	res := s.states.FindOne(ctx, bson.D{
		{Key: "aggregateName", Value: aggregateName},
		{Key: "aggregateId", Value: aggregateID},
	})

	var st state
	if err := res.Decode(&st); err != nil {
		if !errors.Is(err, mongo.ErrNoDocuments) {
			return state{}, fmt.Errorf("decode state: %w", err)
		}
		st = state{
			AggregateName: aggregateName,
			AggregageID:   aggregateID,
		}
	}

	if st.Version >= events[0].AggregateVersion() {
		return st, &VersionError{
			AggregateName:  aggregateName,
			AggregateID:    aggregateID,
			CurrentVersion: st.Version,
			Event:          events[0],
		}
	}

	return st, nil
}

func (s *Store) updateState(ctx mongo.SessionContext, st state, events []event.Event) error {
	if len(events) == 0 || st.AggregateName == "" || st.AggregageID == uuid.Nil {
		return nil
	}
	st.Version = events[len(events)-1].AggregateVersion()
	if _, err := s.states.ReplaceOne(
		ctx,
		bson.D{
			{Key: "aggregateName", Value: st.AggregateName},
			{Key: "aggregateId", Value: st.AggregageID},
		},
		st,
		options.Replace().SetUpsert(true),
	); err != nil {
		return fmt.Errorf("mongo: %w", err)
	}
	return nil
}

func (s *Store) insert(ctx context.Context, evt event.Event) error {
	var data bytes.Buffer
	if err := s.enc.Encode(&data, evt.Data()); err != nil {
		return fmt.Errorf("encode %q event data: %w", evt.Name(), err)
	}

	if _, err := s.entries.InsertOne(ctx, entry{
		ID:               evt.ID(),
		Name:             evt.Name(),
		Time:             evt.Time(),
		TimeNano:         int64(evt.Time().UnixNano()),
		AggregateName:    evt.AggregateName(),
		AggregateID:      evt.AggregateID(),
		AggregateVersion: evt.AggregateVersion(),
		Data:             data.Bytes(),
	}); err != nil {
		return fmt.Errorf("mongo: %w", err)
	}
	return nil
}

// Find returns the Event with the specified UUID from the database if it exists.
func (s *Store) Find(ctx context.Context, id uuid.UUID) (event.Event, error) {
	if err := s.connectOnce(ctx); err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	res := s.entries.FindOne(ctx, bson.M{"id": id})
	var e entry
	if err := res.Decode(&e); err != nil {
		return nil, fmt.Errorf("decode document: %w", err)
	}
	return e.event(s.enc)
}

// Delete deletes the given Event from the database.
func (s *Store) Delete(ctx context.Context, evt event.Event) error {
	if err := s.connectOnce(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if _, err := s.entries.DeleteOne(ctx, bson.M{"id": evt.ID()}); err != nil {
		return fmt.Errorf("delete event %q: %w", evt.ID(), err)
	}
	return nil
}

// Query queries the database for events filtered by Query q and returns an
// event.Stream for those events.
func (s *Store) Query(ctx context.Context, q event.Query) (event.Stream, error) {
	if err := s.connectOnce(ctx); err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	opts := options.Find()
	opts = applySortings(opts, q.Sortings()...)

	cur, err := s.entries.Find(ctx, makeFilter(q), opts)
	if err != nil {
		return nil, fmt.Errorf("mongo: %w", err)
	}
	return &stream{
		enc: s.enc,
		cur: cur,
	}, nil
}

// Connect establishes the connection to the underlying MongoDB and returns the
// mongo.Client. Connect doesn't need to be called manually as it's called
// automatically on the first call to s.Insert, s.Find, s.Delete or s.Query. Use
// Connect if you want to explicitly control when to connect to MongoDB.
func (s *Store) Connect(ctx context.Context, opts ...*options.ClientOptions) (*mongo.Client, error) {
	if err := s.connectOnce(ctx, opts...); err != nil {
		return nil, err
	}
	return s.client, nil
}

func (s *Store) connectOnce(ctx context.Context, opts ...*options.ClientOptions) error {
	var err error
	s.onceConnect.Do(func() {
		if err = s.connect(ctx, opts...); err != nil {
			return
		}
		if err = s.ensureIndexes(ctx); err != nil {
			return
		}
	})
	return err
}

func (s *Store) connect(ctx context.Context, opts ...*options.ClientOptions) error {
	if s.client == nil {
		uri := os.Getenv("MONGO_URL")
		opts = append(
			[]*options.ClientOptions{options.Client().ApplyURI(uri)},
			opts...,
		)

		var err error
		if s.client, err = mongo.Connect(ctx, opts...); err != nil {
			s.client = nil
			return fmt.Errorf("mongo.Connect: %w", err)
		}
	}
	s.db = s.client.Database(s.dbname)
	s.entries = s.db.Collection(s.entriesCol)
	s.states = s.db.Collection(s.statesCol)
	return nil
}

func (s *Store) ensureIndexes(ctx context.Context) error {
	res, err := s.entries.Find(ctx, bson.D{})
	if err != nil {
		return fmt.Errorf("find: %w", err)
	}
	var entries []entry
	if err := res.All(ctx, &entries); err != nil {
		return fmt.Errorf("cursor: %w", err)
	}

	if _, err = s.entries.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "id", Value: 1}},
			Options: options.Index().SetName("goes_id").SetUnique(true),
		},
		{
			Keys:    bson.D{{Key: "name", Value: 1}},
			Options: options.Index().SetName("goes_name"),
		},
		{
			Keys:    bson.D{{Key: "time", Value: 1}},
			Options: options.Index().SetName("goes_time"),
		},
		{
			Keys:    bson.D{{Key: "timeNano", Value: 1}},
			Options: options.Index().SetName("goes_time_nano"),
		},
		{
			Keys: bson.D{
				{Key: "aggregateName", Value: 1},
				{Key: "aggregateId", Value: 1},
				{Key: "aggregateVersion", Value: 1},
			},
			Options: options.Index().
				SetName("goes_aggregate").
				SetUnique(true).
				SetPartialFilterExpression(bson.D{
					{Key: "aggregateName", Value: bson.D{
						{Key: "$gt", Value: ""},
					}},
					{Key: "aggregateId", Value: bson.D{
						{Key: "$gt", Value: uuid.Nil},
					}},
				}),
		},
	}); err != nil {
		return fmt.Errorf("create indexes (%s): %w", s.entries.Name(), err)
	}

	if _, err = s.states.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{
			{Key: "aggregateName", Value: 1},
			{Key: "aggregateId", Value: 1},
		},
		Options: options.Index().SetName("goes_aggregate"),
	}); err != nil {
		return fmt.Errorf("create indexes (%s): %w", s.states.Name(), err)
	}

	return nil
}

func (e entry) event(enc event.Encoder) (event.Event, error) {
	data, err := enc.Decode(e.Name, bytes.NewReader(e.Data))
	if err != nil {
		return nil, fmt.Errorf("decode %q event data: %w", e.Name, err)
	}
	return event.New(
		e.Name,
		data,
		event.ID(e.ID),
		event.Time(stdtime.Unix(0, e.TimeNano)),
		event.Aggregate(e.AggregateName, e.AggregateID, e.AggregateVersion),
	), nil
}

func (c *stream) Next(ctx context.Context) bool {
	if !c.cur.Next(ctx) {
		c.err = c.cur.Err()
		return false
	}

	c.evt = nil
	var e entry
	if c.err = c.cur.Decode(&e); c.err != nil {
		return false
	}

	if c.evt, c.err = e.event(c.enc); c.err != nil {
		return false
	}

	return true
}

func (c *stream) Event() event.Event {
	return c.evt
}

func (c *stream) Err() error {
	return c.err
}

func (c *stream) Close(ctx context.Context) error {
	return c.cur.Close(ctx)
}

func (err *VersionError) Error() string {
	return fmt.Sprintf(
		"event should have version %d, but has version %d",
		err.CurrentVersion+1,
		err.Event.AggregateVersion(),
	)
}

func makeFilter(q event.Query) bson.D {
	filter := make(bson.D, 0)
	filter = withNameFilter(filter, q.Names()...)
	filter = withIDFilter(filter, q.IDs()...)
	filter = withTimeFilter(filter, q.Times())
	filter = withAggregateNameFilter(filter, q.AggregateNames()...)
	filter = withAggregateIDFilter(filter, q.AggregateIDs()...)
	filter = withAggregateVersionFilter(filter, q.AggregateVersions())
	return filter
}

func withNameFilter(filter bson.D, names ...string) bson.D {
	if len(names) == 0 {
		return filter
	}
	return append(filter, bson.E{
		Key: "name", Value: bson.D{{Key: "$in", Value: names}},
	})
}

func withIDFilter(filter bson.D, ids ...uuid.UUID) bson.D {
	if len(ids) == 0 {
		return filter
	}
	return append(filter, bson.E{
		Key: "id", Value: bson.D{{Key: "$in", Value: ids}},
	})
}

func withTimeFilter(filter bson.D, times time.Constraints) bson.D {
	if times == nil {
		return filter
	}

	if exact := times.Exact(); len(exact) > 0 {
		filter = append(filter, bson.E{
			Key:   "timeNano",
			Value: bson.D{{Key: "$in", Value: nanoTimes(exact...)}},
		})
	}

	if min := times.Min(); !min.IsZero() {
		filter = append(filter, bson.E{Key: "timeNano", Value: bson.D{{
			Key: "$gte", Value: min.UnixNano(),
		}}})
	}

	if max := times.Max(); !max.IsZero() {
		filter = append(filter, bson.E{Key: "timeNano", Value: bson.D{{
			Key: "$lte", Value: max.UnixNano(),
		}}})
	}

	if ranges := times.Ranges(); len(ranges) > 0 {
		var or []bson.D
		for _, r := range ranges {
			or = append(or, bson.D{{Key: "timeNano", Value: bson.D{
				{Key: "$gte", Value: r.Start()},
				{Key: "$lte", Value: r.End()},
			}}})
		}
		filter = append(filter, bson.E{Key: "$or", Value: or})
	}

	return filter
}

func nanoTimes(ts ...stdtime.Time) []int64 {
	nano := make([]int64, len(ts))
	for i, t := range ts {
		nano[i] = t.UnixNano()
	}
	return nano
}

func withAggregateNameFilter(filter bson.D, names ...string) bson.D {
	if len(names) == 0 {
		return filter
	}
	return append(filter, bson.E{
		Key: "aggregateName", Value: bson.D{{Key: "$in", Value: names}},
	})
}

func withAggregateIDFilter(filter bson.D, ids ...uuid.UUID) bson.D {
	if len(ids) == 0 {
		return filter
	}
	return append(filter, bson.E{
		Key: "aggregateId", Value: bson.D{{Key: "$in", Value: ids}},
	})
}

func withAggregateVersionFilter(filter bson.D, versions version.Constraints) bson.D {
	if versions == nil {
		return filter
	}

	if exact := versions.Exact(); len(exact) > 0 {
		filter = append(filter, bson.E{
			Key: "aggregateVersion", Value: bson.D{{Key: "$in", Value: exact}},
		})
	}

	if min := versions.Min(); len(min) > 0 {
		var or []bson.D
		for _, v := range min {
			or = append(or, bson.D{{
				Key: "aggregateVersion", Value: bson.D{{Key: "$gte", Value: v}},
			}})
		}
		filter = append(filter, bson.E{Key: "$or", Value: or})
	}

	if max := versions.Max(); len(max) > 0 {
		var or []bson.D
		for _, v := range max {
			or = append(or, bson.D{{
				Key: "aggregateVersion", Value: bson.D{{Key: "$lte", Value: v}},
			}})
		}
		filter = append(filter, bson.E{Key: "$or", Value: or})
	}

	if ranges := versions.Ranges(); len(ranges) > 0 {
		var or []bson.D
		for _, r := range ranges {
			or = append(or, bson.D{{Key: "aggregateVersion", Value: bson.D{
				{Key: "$gte", Value: r.Start()},
				{Key: "$lte", Value: r.End()},
			}}})
		}
		filter = append(filter, bson.E{Key: "$or", Value: or})
	}

	return filter
}

func applySortings(opts *options.FindOptions, sortings ...event.SortOptions) *options.FindOptions {
	sorts := make(bson.D, len(sortings))
	for i, opts := range sortings {
		v := 1
		if !opts.Dir.Bool(true) {
			v = -1
		}

		switch opts.Sort {
		case event.SortTime:
			sorts[i] = bson.E{Key: "timeNano", Value: v}
		case event.SortAggregateName:
			sorts[i] = bson.E{Key: "aggregateName", Value: v}
		case event.SortAggregateID:
			sorts[i] = bson.E{Key: "aggregateId", Value: v}
		case event.SortAggregateVersion:
			sorts[i] = bson.E{Key: "aggregateVersion", Value: v}
		}
	}
	return opts.SetSort(sorts)
}
