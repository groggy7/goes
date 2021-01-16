// +build nats

package nats

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/modernice/goes/event"
	"github.com/modernice/goes/event/eventbus/test"
	"github.com/nats-io/nats.go"
)

func TestQueueGroupByFunc(t *testing.T) {
	// given an event bus with a queue group func
	bus := New(test.NewEncoder(), QueueGroupByFunc(func(eventName string) string {
		return fmt.Sprintf("bar.%s", eventName)
	}))

	// given 5 "foo" subscribers
	subs := make([]<-chan event.Event, 5)
	for i := range subs {
		events, err := bus.Subscribe(context.Background(), "foo")
		if err != nil {
			t.Fatal(fmt.Errorf("[%d] subscribe to %q events: %w", i, "foo", err))
		}
		subs[i] = events
	}

	// when a "foo" event is published
	evt := event.New("foo", test.FooEventData{})
	err := bus.Publish(context.Background(), evt)
	if err != nil {
		t.Fatal(fmt.Errorf("publish event %#v: %w", evt, err))
	}

	// only 1 subscriber should received the event
	receivedChan := make(chan event.Event, len(subs))
	var wg sync.WaitGroup
	wg.Add(len(subs))
	go func() {
		defer close(receivedChan)
		wg.Wait()
	}()
	for _, events := range subs {
		go func(events <-chan event.Event) {
			defer wg.Done()
			select {
			case evt := <-events:
				receivedChan <- evt
			case <-time.After(100 * time.Millisecond):
			}
		}(events)
	}
	wg.Wait()

	var received []event.Event
	for evt := range receivedChan {
		received = append(received, evt)
	}

	if len(received) != 1 {
		t.Fatal(fmt.Errorf("expected exactly 1 subscriber to receive an event; %d subscribers received it", len(received)))
	}

	if !event.Equal(received[0], evt) {
		t.Fatal(fmt.Errorf("received event doesn't match published event\npublished: %#v\n\nreceived: %#v", evt, received[0]))
	}
}

func TestQueueGroupByEvent(t *testing.T) {
	bus := New(test.NewEncoder(), QueueGroupByEvent())
	names := []string{"foo", "bar", "baz"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			if queue := bus.queueFunc(name); queue != name {
				t.Fatal(fmt.Errorf("expected queueFunc to return %q; got %q", name, queue))
			}
		})
	}
}

func TestConnectWith(t *testing.T) {
	bus := New(test.NewEncoder(), ConnectWith(
		func(opts *nats.Options) error {
			opts.AllowReconnect = true
			return nil
		},
		func(opts *nats.Options) error {
			opts.MaxReconnect = 4
			return nil
		},
	))

	var opts nats.Options
	for _, opt := range bus.connectOpts {
		opt(&opts)
	}
	if !opts.AllowReconnect {
		t.Error(fmt.Errorf("expected AllowReconnect option to be %v; got %v", true, opts.AllowReconnect))
	}
	if opts.MaxReconnect != 4 {
		t.Error(fmt.Errorf("expected MaxReconnect option to be %v; got %v", 4, opts.MaxReconnect))
	}
}

func TestURL(t *testing.T) {
	url := "foo://bar:123"
	bus := New(test.NewEncoder(), URL(url))
	if bus.natsURL() != url {
		t.Fatal(fmt.Errorf("expected bus.natsURL to return %q; got %q", url, bus.natsURL()))
	}
}

func TestEventBus_natsURL(t *testing.T) {
	envURL := "foo://bar:123"
	org := os.Getenv("NATS_URL")
	if err := os.Setenv("NATS_URL", envURL); err != nil {
		t.Fatal(fmt.Errorf("set env %q=%q: %w", "NATS_URL", envURL, err))
	}
	defer func() {
		if err := os.Setenv("NATS_URL", org); err != nil {
			t.Fatal(fmt.Errorf("set env %q=%q: %w", "NATS_URL", org, err))
		}
	}()

	bus := New(test.NewEncoder())
	if bus.natsURL() != envURL {
		t.Fatal(fmt.Errorf("expected bus.natsURL to return %q; got %q", envURL, bus.natsURL()))
	}

	bus.url = "bar://foo:321"
	if bus.natsURL() != bus.url {
		t.Fatal(fmt.Errorf("expected bus.natsURL to return %q; got %q", bus.url, bus.natsURL()))
	}
}

func TestConnection(t *testing.T) {
	conn := &nats.Conn{}
	bus := New(test.NewEncoder(), Connection(conn))

	if bus.conn != conn {
		t.Fatal(fmt.Errorf("expected bus.conn to be %#v; got %#v", conn, bus.conn))
	}

	if err := bus.connectOnce(context.Background()); err != nil {
		t.Fatal(fmt.Errorf("expected bus.connectOnce not to fail; got %#v", err))
	}

	if bus.conn != conn {
		t.Fatal(fmt.Errorf("expected bus.conn to still be %#v; got %#v", conn, bus.conn))
	}
}

func TestSubjectFunc(t *testing.T) {
	bus := New(test.NewEncoder(), SubjectFunc(func(eventName string) string {
		return "prefix." + eventName
	}))

	events, err := bus.Subscribe(context.Background(), "foo")
	if err != nil {
		t.Fatal(fmt.Errorf("subscribe to %q events: %w", "foo", err))
	}

	evt := event.New("foo", test.FooEventData{A: "foo"})
	if err = bus.Publish(context.Background(), evt); err != nil {
		t.Fatal(fmt.Errorf("publish %q event: %w", "foo", err))
	}

	timeout := time.NewTimer(time.Second)
	for {
		select {
		case <-timeout.C:
			t.Fatal(fmt.Errorf("didn't receive event after 1s"))
		case received := <-events:
			if !event.Equal(received, evt) {
				t.Fatal(fmt.Errorf("expected received event to equal %#v; got %#v", evt, received))
			}
			return
		}
	}
}

func TestSubjectFunc_subjectFunc(t *testing.T) {
	// default subject
	bus := New(test.NewEncoder())
	if got := bus.subjectFunc("foo"); got != "foo" {
		t.Fatal(fmt.Errorf("expected bus.subjectFunc(%q) to return %q; got %q", "foo", "foo", got))
	}

	// custom subject func
	bus = New(test.NewEncoder(), SubjectFunc(func(eventName string) string {
		return fmt.Sprintf("prefix.%s", eventName)
	}))

	want := "prefix.foo"
	if got := bus.subjectFunc("foo"); got != want {
		t.Fatal(fmt.Errorf("expected bus.subjectFunc(%q) to return %q; got %q", "foo", want, got))
	}
}

func TestSubjectPrefix(t *testing.T) {
	bus := New(test.NewEncoder(), SubjectPrefix("prefix."))

	want := "prefix.foo"
	if got := bus.subjectFunc("foo"); got != want {
		t.Fatal(fmt.Errorf("expected bus.subjectFunc(%q) to return %q; got %q", "foo", want, got))
	}
}
