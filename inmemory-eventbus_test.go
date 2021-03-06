package cqrs_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/andrewwebber/cqrs"
)

type SampleEvent struct {
	Message string
}

func TestInMemoryEventBus(t *testing.T) {
	bus := cqrs.NewInMemoryEventBus()
	eventType := reflect.TypeOf(SampleEvent{})

	// Create communication channels
	//
	// for closing the queue listener,
	closeChannel := make(chan chan error)
	// receiving errors from the listener thread (go routine)
	errorChannel := make(chan error)
	// and receiving events from the queue
	receiveEventChannel := make(chan cqrs.VersionedEventTransactedAccept)
	eventHandler := func(event cqrs.VersionedEvent) error {
		accepted := make(chan bool)
		receiveEventChannel <- cqrs.VersionedEventTransactedAccept{Event: event, ProcessedSuccessfully: accepted}
		if <-accepted {
			return nil
		}

		return errors.New("Unsuccessful")
	}

	// Start receiving events by passing these channels to the worker thread (go routine)
	if err := bus.ReceiveEvents(cqrs.VersionedEventReceiverOptions{TypeRegistry: nil, Close: closeChannel, Error: errorChannel, ReceiveEvent: eventHandler}); err != nil {
		t.Fatal(err)
	}

	// Publish a simple event
	cqrs.PackageLogger().Debugf("Publishing events")
	go func() {
		if err := bus.PublishEvents([]cqrs.VersionedEvent{{
			EventType: eventType.String(),
			Event:     SampleEvent{"TestInMemoryEventBus"}}}); err != nil {
			t.Fatal(err)
		}
	}()

	// If we dont receive a message within 5 seconds this test is a failure. Use a channel to signal the timeout
	timeout := make(chan bool, 1)
	go func() {
		time.Sleep(5 * time.Second)
		timeout <- true
	}()

	// Wait on multiple channels using the select control flow.
	select {
	// Test timeout
	case <-timeout:
		t.Fatal("Test timed out")
		// Version event received channel receives a result with a channel to respond to, signifying successful processing of the message.
		// This should eventually call an event handler. See cqrs.NewVersionedEventDispatcher()
	case event := <-receiveEventChannel:
		sampleEvent := event.Event.Event.(SampleEvent)
		cqrs.PackageLogger().Debugf(sampleEvent.Message)
		event.ProcessedSuccessfully <- true
		// Receiving on this channel signifys an error has occured work processor side
	case err := <-errorChannel:
		t.Fatal(err)
	}

	closeResponse := make(chan error)
	closeChannel <- closeResponse
	<-closeResponse
}
