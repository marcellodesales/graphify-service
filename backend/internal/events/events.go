// Package events is the NATS JetStream eventing layer for the async pipeline
// (spec §8, PRD-001). It publishes/consumes CloudEvents-enveloped repository
// lifecycle events with idempotent delivery (Nats-Msg-Id dedup).
package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// Stream + subjects (spec §8.2).
const (
	StreamName    = "GRAPHIFY_JOBS"
	subjectFilter = "graphify.repository.>"

	SubjectCloneRequested = "graphify.repository.clone.requested.v1"
	SubjectCloned         = "graphify.repository.cloned.v1"
	SubjectCloneFailed    = "graphify.repository.clone.failed.v1"
	SubjectGraphStarted   = "graphify.repository.graph.started.v1"
	SubjectGraphReady     = "graphify.repository.graph.ready.v1"
	SubjectGraphFailed    = "graphify.repository.graph.failed.v1"
)

// Durable consumer names (spec §8.4).
const (
	DurableCloneWorker = "graphify-clone-workers-v1"
	DurableGraphWorker = "graphify-graph-workers-v1"
)

// RepoEventData is the non-secret payload carried by every repository event.
type RepoEventData struct {
	RepositoryID  string `json:"repositoryId"`
	SelectorType  string `json:"selectorType,omitempty"`
	SelectorValue string `json:"selectorValue,omitempty"`
	ResolvedSHA   string `json:"resolvedSha,omitempty"`
	Message       string `json:"message,omitempty"`
}

// CloudEvent is a minimal CloudEvents 1.0 envelope.
type CloudEvent struct {
	SpecVersion     string        `json:"specversion"`
	ID              string        `json:"id"`
	Source          string        `json:"source"`
	Type            string        `json:"type"`
	Subject         string        `json:"subject"`
	Time            string        `json:"time"`
	DataContentType string        `json:"datacontenttype"`
	Data            RepoEventData `json:"data"`
}

// Bus is a JetStream publisher/subscriber.
type Bus struct {
	nc     *nats.Conn
	js     nats.JetStreamContext
	source string
}

// Connect dials NATS, ensures the stream exists, and returns a Bus. source is a
// URN identifying the publishing service (e.g. "urn:graphify-service:api").
func Connect(url, source string) (*Bus, error) {
	nc, err := nats.Connect(url,
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.Name(source),
	)
	if err != nil {
		return nil, fmt.Errorf("events: connect %q: %w", url, err)
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("events: jetstream: %w", err)
	}
	b := &Bus{nc: nc, js: js, source: source}
	if err := b.ensureStream(); err != nil {
		nc.Close()
		return nil, err
	}
	return b, nil
}

func (b *Bus) ensureStream() error {
	if _, err := b.js.StreamInfo(StreamName); err == nil {
		return nil
	}
	_, err := b.js.AddStream(&nats.StreamConfig{
		Name:       StreamName,
		Subjects:   []string{subjectFilter},
		Retention:  nats.LimitsPolicy,
		Storage:    nats.FileStorage,
		MaxAge:     24 * time.Hour,
		Duplicates: 5 * time.Minute, // Nats-Msg-Id dedup window
	})
	if err != nil {
		return fmt.Errorf("events: add stream: %w", err)
	}
	return nil
}

// Connected reports whether the NATS connection is live (for readiness).
func (b *Bus) Connected() bool { return b.nc != nil && b.nc.IsConnected() }

// Close drains and closes the connection.
func (b *Bus) Close() {
	if b.nc != nil {
		_ = b.nc.Drain()
	}
}

// Publish emits a CloudEvent on subject. msgID is the idempotency key
// (Nats-Msg-Id) — JetStream drops duplicates within the dedup window.
func (b *Bus) Publish(subject, msgID string, data RepoEventData) error {
	ev := CloudEvent{
		SpecVersion:     "1.0",
		ID:              msgID,
		Source:          b.source,
		Type:            cloudType(subject),
		Subject:         "repository/" + data.RepositoryID,
		Time:            time.Now().UTC().Format(time.RFC3339),
		DataContentType: "application/json",
		Data:            data,
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("events: encode: %w", err)
	}
	if _, err := b.js.Publish(subject, payload, nats.MsgId(msgID)); err != nil {
		return fmt.Errorf("events: publish %s: %w", subject, err)
	}
	return nil
}

// Handler processes an event's data. Returning an error nak's the message for
// redelivery; returning nil acks it.
type Handler func(RepoEventData) error

// Subscribe creates (or binds) a durable push consumer on subject and invokes
// handler for each message with explicit ack.
func (b *Bus) Subscribe(subject, durable string, handler Handler) (*nats.Subscription, error) {
	sub, err := b.js.Subscribe(subject, func(m *nats.Msg) {
		var ev CloudEvent
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			_ = m.Term() // unparseable — don't redeliver
			return
		}
		if err := handler(ev.Data); err != nil {
			_ = m.Nak()
			return
		}
		_ = m.Ack()
	},
		nats.Durable(durable),
		nats.ManualAck(),
		nats.AckExplicit(),
		nats.DeliverAll(),
		nats.AckWait(2*time.Minute),
		nats.MaxDeliver(5),
	)
	if err != nil {
		return nil, fmt.Errorf("events: subscribe %s: %w", subject, err)
	}
	return sub, nil
}

func cloudType(subject string) string {
	// graphify.repository.clone.requested.v1 -> com.graphify.repository.clone.requested.v1
	return "com." + subject
}
