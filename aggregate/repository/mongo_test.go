//go:build mongo

package repository_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modernice/goes/aggregate"
	"github.com/modernice/goes/aggregate/repository"
	"github.com/modernice/goes/backend/mongo"
	"github.com/modernice/goes/event"
	etest "github.com/modernice/goes/event/test"
)

func TestRepository_Use_Retry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	enc := etest.NewEncoder()
	estore := mongo.NewEventStore(enc, mongo.URL(os.Getenv("MONGOSTORE_URL")))

	r := repository.New(estore)

	foo := newRetryer()

	events := []event.Event{
		aggregate.Next(foo, "foo", etest.FooEventData{}).Any(),
		aggregate.Next(foo, "foo", etest.FooEventData{}).Any(),
		aggregate.Next(foo, "foo", etest.FooEventData{}).Any(),
	}

	aggregate.ApplyHistory(foo, events)

	r.Save(ctx, foo)

	start := time.Now()
	var tries int
	if err := r.Use(ctx, foo, func() error {
		tries++
		// apply the last event again. this should fail with a *mongo.VersionError
		foo.RecordChange(events[len(events)-1])
		return nil
	}); !aggregate.IsConsistencyError(err) {
		t.Fatalf("Use() should fail with a consistency error; got %T %v", err, err)
	}

	if dur := time.Since(start); dur.Milliseconds() < 150 || dur.Milliseconds() > 250 {
		t.Fatalf("Use() should have taken ~%v; took %v", 150*time.Millisecond, dur)
	}

	if tries != 4 {
		t.Fatalf("Use() should have tried 4 times; tried %d times", tries)
	}
}

type retryer struct{ *aggregate.Base }

func newRetryer() *retryer {
	return &retryer{
		Base: aggregate.New("retryer", uuid.New()),
	}
}

// RetryUse returns a RetryTrigger and an IsRetryable function for the retryer.
// The RetryTrigger specifies to retry every 50 milliseconds for a maximum of 4
// times, and the IsRetryable function checks if an error is a consistency
// error.
func (r *retryer) RetryUse() (repository.RetryTrigger, repository.IsRetryable) {
	return repository.RetryEvery(50*time.Millisecond, 4), aggregate.IsConsistencyError
}
