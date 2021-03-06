package cqrs

import (
	"errors"
	"reflect"
	"time"
)

// ErrConcurrencyWhenSavingEvents is raised when a concurrency error has occured when saving events
var ErrConcurrencyWhenSavingEvents = errors.New("concurrency error saving event")

// ErrNonePendingWhenSavingEvents is raised when a save is issued but no events are pending for the eventsourced entity.
var ErrNonePendingWhenSavingEvents = errors.New("no events pending error saving event")

// VersionedEvent represents an event in the past for an aggregate
type VersionedEvent struct {
	ID            string    `json:"id"`
	CorrelationID string    `json:"correlationID"`
	SourceID      string    `json:"sourceID"`
	Actor         string    `json:"actor"`
	OnBehalfOf    string    `json:"onbehalfof"`
	Version       int       `json:"version"`
	EventType     string    `json:"eventType"`
	Created       time.Time `json:"time"`
	Event         interface{}
}

// ByCreated is an alias for sorting VersionedEvents by the create field
type ByCreated []VersionedEvent

func (c ByCreated) Len() int           { return len(c) }
func (c ByCreated) Swap(i, j int)      { c[i], c[j] = c[j], c[i] }
func (c ByCreated) Less(i, j int) bool { return c[i].Created.Before(c[j].Created) }

// VersionedEventPublicationLogger is responsible to retreiving all events ever published to facilitate readmodel reconstruction
type VersionedEventPublicationLogger interface {
	SaveIntegrationEvent(VersionedEvent) error
	AllIntegrationEventsEverPublished() ([]VersionedEvent, error)
	GetIntegrationEventsByCorrelationID(correlationID string) ([]VersionedEvent, error)
}

// VersionedEventPublisher is responsible for publishing events that have been saved to the event store\repository
type VersionedEventPublisher interface {
	PublishEvents([]VersionedEvent) error
}

// VersionedEventReceiver is responsible for receiving globally published events
type VersionedEventReceiver interface {
	ReceiveEvents(VersionedEventReceiverOptions) error
}

// EventBus ...
type EventBus interface {
	VersionedEventPublisher
	VersionedEventReceiver
}

// VersionedEventDispatchManager is responsible for coordinating receiving messages from event receivers and dispatching them to the event dispatcher.
type VersionedEventDispatchManager struct {
	versionedEventDispatcher *MapBasedVersionedEventDispatcher
	typeRegistry             TypeRegistry
	receiver                 VersionedEventReceiver
}

// VersionedEventDispatcher the internal versioned event dispatcher
func (m *VersionedEventDispatchManager) VersionedEventDispatcher() VersionedEventDispatcher {
	return m.versionedEventDispatcher
}

// VersionedEventTransactedAccept is the message routed from an event receiver to the event manager.
// Sometimes event receivers designed with reliable delivery require acknowledgements after a message has been received. The success channel here allows for such acknowledgements
type VersionedEventTransactedAccept struct {
	Event                 VersionedEvent
	ProcessedSuccessfully chan bool
}

// VersionedEventReceiverOptions is an initalization structure to communicate to and from an event receiver go routine
type VersionedEventReceiverOptions struct {
	TypeRegistry  TypeRegistry
	Close         chan chan error
	Error         chan error
	ReceiveEvent  VersionedEventHandler
	Exclusive     bool
	ListenerCount int
}

// VersionedEventDispatcher is responsible for routing events from the event manager to call handlers responsible for processing received events
type VersionedEventDispatcher interface {
	DispatchEvent(VersionedEvent) error
	RegisterEventHandler(event interface{}, handler VersionedEventHandler)
	RegisterGlobalHandler(handler VersionedEventHandler)
}

// MapBasedVersionedEventDispatcher is a simple implementation of the versioned event dispatcher. Using a map it registered event handlers to event types
type MapBasedVersionedEventDispatcher struct {
	registry       map[reflect.Type][]VersionedEventHandler
	globalHandlers []VersionedEventHandler
}

// VersionedEventHandler is a function that takes a versioned event
type VersionedEventHandler func(VersionedEvent) error

// NewVersionedEventDispatcher is a constructor for the MapBasedVersionedEventDispatcher
func NewVersionedEventDispatcher() *MapBasedVersionedEventDispatcher {
	registry := make(map[reflect.Type][]VersionedEventHandler)
	return &MapBasedVersionedEventDispatcher{registry, []VersionedEventHandler{}}
}

// RegisterEventHandler allows a caller to register an event handler given an event of the specified type being received
func (m *MapBasedVersionedEventDispatcher) RegisterEventHandler(event interface{}, handler VersionedEventHandler) {
	eventType := reflect.TypeOf(event)
	handlers, ok := m.registry[eventType]
	if ok {
		m.registry[eventType] = append(handlers, handler)
	} else {
		m.registry[eventType] = []VersionedEventHandler{handler}
	}
}

// RegisterGlobalHandler allows a caller to register a wildcard event handler call on any event received
func (m *MapBasedVersionedEventDispatcher) RegisterGlobalHandler(handler VersionedEventHandler) {
	m.globalHandlers = append(m.globalHandlers, handler)
}

// DispatchEvent executes all event handlers registered for the given event type
func (m *MapBasedVersionedEventDispatcher) DispatchEvent(event VersionedEvent) error {
	eventType := reflect.TypeOf(event.Event)
	if handlers, ok := m.registry[eventType]; ok {
		for _, handler := range handlers {
			if err := handler(event); err != nil {
				metricsEventsFailed.WithLabelValues(event.EventType).Inc()
				return err
			}
		}
	}

	for _, handler := range m.globalHandlers {
		if err := handler(event); err != nil {
			metricsEventsFailed.WithLabelValues(event.EventType).Inc()
			return err
		}
	}

	metricsEventsDispatched.WithLabelValues(event.EventType).Inc()

	return nil
}

// NewVersionedEventDispatchManager is a constructor for the VersionedEventDispatchManager
func NewVersionedEventDispatchManager(receiver VersionedEventReceiver, registry TypeRegistry) *VersionedEventDispatchManager {
	return &VersionedEventDispatchManager{NewVersionedEventDispatcher(), registry, receiver}
}

// RegisterEventHandler allows a caller to register an event handler given an event of the specified type being received
func (m *VersionedEventDispatchManager) RegisterEventHandler(event interface{}, handler VersionedEventHandler) {
	m.typeRegistry.RegisterType(event)
	m.versionedEventDispatcher.RegisterEventHandler(event, handler)
}

// RegisterGlobalHandler allows a caller to register a wildcard event handler call on any event received
func (m *VersionedEventDispatchManager) RegisterGlobalHandler(handler VersionedEventHandler) {
	m.versionedEventDispatcher.RegisterGlobalHandler(handler)
}

// Listen starts a listen loop processing channels related to new incoming events, errors and stop listening requests
func (m *VersionedEventDispatchManager) Listen(stop <-chan bool, exclusive bool, listenerCount int) error {
	// Create communication channels
	//
	// for closing the queue listener,
	closeChannel := make(chan chan error)
	// receiving errors from the listener thread (go routine)
	errorChannel := make(chan error)

	// Version event received channel receives a result with a channel to respond to, signifying successful processing of the message.
	// This should eventually call an event handler. See cqrs.NewVersionedEventDispatcher()
	versionedEventHandler := func(event VersionedEvent) error {
		err := m.versionedEventDispatcher.DispatchEvent(event)
		if err != nil {
			PackageLogger().Debugf("Error dispatching event: %v", err)
		}

		return err
	}

	// Start receiving events by passing these channels to the worker thread (go routine)
	options := VersionedEventReceiverOptions{m.typeRegistry, closeChannel, errorChannel, versionedEventHandler, exclusive, listenerCount}
	if err := m.receiver.ReceiveEvents(options); err != nil {
		return err
	}

	go func() {
		for {
			// Wait on multiple channels using the select control flow.
			select {
			//PackageLogger().Debugf(nil, "EventDispatchManager.DispatchSuccessful")
			case <-stop:
				PackageLogger().Debugf("EventDispatchManager.Stopping")
				closeSignal := make(chan error)
				closeChannel <- closeSignal
				PackageLogger().Debugf("EventDispatchManager.Stopped")
				<-closeSignal
			// Receiving on this channel signifys an error has occured worker processor side
			case err := <-errorChannel:
				PackageLogger().Debugf("EventDispatchManager.ErrorReceived: %v", err)
			}
		}
	}()

	return nil
}
