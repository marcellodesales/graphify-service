package repository

// Status is a repository job lifecycle state (spec §7.1).
type Status string

const (
	StatusQueued      Status = "queued"
	StatusCloning     Status = "cloning"
	StatusCloned      Status = "cloned"
	StatusGraphifying Status = "graphifying"
	StatusReady       Status = "ready"
	StatusFailed      Status = "failed"
)

// validTransitions encodes the allowed state machine edges (spec §7.2).
// The failed -> queued edge is only reachable through an explicit retry
// operation (not implemented in Phase 1) and is intentionally excluded here.
var validTransitions = map[Status]map[Status]bool{
	StatusQueued:      {StatusCloning: true},
	StatusCloning:     {StatusCloned: true, StatusFailed: true},
	StatusCloned:      {StatusGraphifying: true},
	StatusGraphifying: {StatusReady: true, StatusFailed: true},
	StatusReady:       {},
	StatusFailed:      {},
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	_, ok := validTransitions[s]
	return ok
}

// Terminal reports whether s is an end state (ready or failed).
func (s Status) Terminal() bool {
	return s == StatusReady || s == StatusFailed
}

// CanTransition reports whether from -> to is a permitted transition.
func CanTransition(from, to Status) bool {
	return validTransitions[from][to]
}
