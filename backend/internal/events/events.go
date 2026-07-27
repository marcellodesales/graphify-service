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
//
// The stream captures the whole graphify.* namespace so both the single-repo
// pipeline (graphify.repository.*) and the memory pipeline (graphify.memory.*)
// share one JetStream stream and dedup window.
const (
	StreamName    = "GRAPHIFY_JOBS"
	subjectFilter = "graphify.>"

	SubjectCloneRequested = "graphify.repository.clone.requested.v1"
	SubjectCloned         = "graphify.repository.cloned.v1"
	SubjectCloneFailed    = "graphify.repository.clone.failed.v1"
	SubjectGraphStarted   = "graphify.repository.graph.started.v1"
	SubjectGraphReady     = "graphify.repository.graph.ready.v1"
	SubjectGraphFailed    = "graphify.repository.graph.failed.v1"

	// Memory pipeline (multi-source unified graph). A resource is one source
	// (git repo or uploaded file) within a memory; merge combines all resource
	// graphs into the memory's unified graph.
	SubjectMemoryResourceRequested = "graphify.memory.resource.requested.v1"
	SubjectMemoryResourceReady     = "graphify.memory.resource.ready.v1"
	SubjectMemoryResourceFailed    = "graphify.memory.resource.failed.v1"
	SubjectMemoryMergeRequested    = "graphify.memory.merge.requested.v1"
	SubjectMemoryMergeReady        = "graphify.memory.merge.ready.v1"
	SubjectMemoryMergeFailed       = "graphify.memory.merge.failed.v1"
)

// Durable consumer names (spec §8.4).
const (
	DurableCloneWorker = "graphify-clone-workers-v1"
	DurableGraphWorker = "graphify-graph-workers-v1"

	DurableMemoryResourceWorker = "graphify-memory-resource-workers-v1"
	DurableMemoryMergeWorker    = "graphify-memory-merge-workers-v1"
)

// RepoEventData is the non-secret payload carried by every repository event.
type RepoEventData struct {
	RepositoryID  string `json:"repositoryId"`
	SelectorType  string `json:"selectorType,omitempty"`
	SelectorValue string `json:"selectorValue,omitempty"`
	ResolvedSHA   string `json:"resolvedSha,omitempty"`
	Message       string `json:"message,omitempty"`
}

// MemoryEventData is the non-secret payload carried by every memory event.
// ResourceID is set on resource-scoped events; GraphRef carries the memory
// repo's HEAD SHA after a merge completes.
type MemoryEventData struct {
	MemoryID    string `json:"memoryId"`
	ResourceID  string `json:"resourceId,omitempty"`
	ResolvedSHA string `json:"resolvedSha,omitempty"`
	GraphRef    string `json:"graphRef,omitempty"`
	Message     string `json:"message,omitempty"`
}

// CloudEvent is a minimal CloudEvents 1.0 envelope. Data is kept as raw JSON so
// one envelope carries either a RepoEventData or a MemoryEventData payload; the
// typed Publish/Subscribe helpers marshal and decode it.
type CloudEvent struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject"`
	Time            string          `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	Data            json.RawMessage `json:"data"`
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
	if info, err := b.js.StreamInfo(StreamName); err == nil {
		// The stream exists. An older deploy may have created it with the narrow
		// graphify.repository.> filter that does not capture memory subjects;
		// widen it in place so this build's memory events are retained.
		for _, s := range info.Config.Subjects {
			if s == subjectFilter {
				return nil
			}
		}
		cfg := info.Config
		cfg.Subjects = []string{subjectFilter}
		if _, err := b.js.UpdateStream(&cfg); err != nil {
			return fmt.Errorf("events: update stream subjects: %w", err)
		}
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

// Publish emits a repository CloudEvent on subject. msgID is the idempotency key
// (Nats-Msg-Id) — JetStream drops duplicates within the dedup window.
func (b *Bus) Publish(subject, msgID string, data RepoEventData) error {
	return b.publish(subject, msgID, "repository/"+data.RepositoryID, data)
}

// PublishMemory emits a memory CloudEvent on subject. The envelope subject is
// scoped as "memory/<memoryId>".
func (b *Bus) PublishMemory(subject, msgID string, data MemoryEventData) error {
	return b.publish(subject, msgID, "memory/"+data.MemoryID, data)
}

// publish marshals data into the envelope and publishes it idempotently.
func (b *Bus) publish(subject, msgID, subjectRef string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("events: encode data: %w", err)
	}
	ev := CloudEvent{
		SpecVersion:     "1.0",
		ID:              msgID,
		Source:          b.source,
		Type:            cloudType(subject),
		Subject:         subjectRef,
		Time:            time.Now().UTC().Format(time.RFC3339),
		DataContentType: "application/json",
		Data:            raw,
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

// Handler processes a repository event's data. Returning an error nak's the
// message for redelivery; returning nil acks it.
type Handler func(RepoEventData) error

// MemoryHandler processes a memory event's data (same ack semantics).
type MemoryHandler func(MemoryEventData) error

// Subscribe creates (or binds) a durable push consumer on subject and invokes
// handler for each repository message with explicit ack.
func (b *Bus) Subscribe(subject, durable string, ackWait time.Duration, handler Handler) (*nats.Subscription, error) {
	return b.subscribe(subject, durable, ackWait, func(raw json.RawMessage) error {
		var data RepoEventData
		if err := json.Unmarshal(raw, &data); err != nil {
			return errTerm{err}
		}
		return handler(data)
	})
}

// SubscribeMemory creates (or binds) a durable push consumer on subject and
// invokes handler for each memory message with explicit ack.
func (b *Bus) SubscribeMemory(subject, durable string, ackWait time.Duration, handler MemoryHandler) (*nats.Subscription, error) {
	return b.subscribe(subject, durable, ackWait, func(raw json.RawMessage) error {
		var data MemoryEventData
		if err := json.Unmarshal(raw, &data); err != nil {
			return errTerm{err}
		}
		return handler(data)
	})
}

// errTerm marks a decode error that must terminate (not redeliver) the message.
type errTerm struct{ err error }

func (e errTerm) Error() string { return e.err.Error() }

func (b *Bus) subscribe(subject, durable string, ackWait time.Duration, decode func(json.RawMessage) error) (*nats.Subscription, error) {
	sub, err := b.js.Subscribe(subject, func(m *nats.Msg) {
		var ev CloudEvent
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			_ = m.Term() // unparseable envelope — don't redeliver
			return
		}
		if err := decode(ev.Data); err != nil {
			if _, ok := err.(errTerm); ok {
				_ = m.Term() // unparseable data — poison, don't redeliver
				return
			}
			_ = m.Nak()
			return
		}
		_ = m.Ack()
	},
		nats.Durable(durable),
		nats.ManualAck(),
		nats.AckExplicit(),
		nats.DeliverAll(),
		nats.AckWait(ackWait),
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
