package memory

// Status is the lifecycle phase of a memory.
//
// Unlike a repository (a one-shot content-addressed pipeline), a memory is
// mutable: resources can be added at any time, which drives it back through
// ingesting/merging even after it has reached ready. The transition table is
// therefore intentionally permissive about re-entry — reaching ready is not
// terminal, and a fresh mutation may reopen the pipeline.
type Status string

const (
	// StatusCreated: the memory exists (has an ID and a git repo) but has no
	// resources yet, or none have been graphified.
	StatusCreated Status = "created"
	// StatusIngesting: one or more resources are being cloned/uploaded and
	// graphify-extracted.
	StatusIngesting Status = "ingesting"
	// StatusMerging: all resource graphs are being merged into the unified graph.
	StatusMerging Status = "merging"
	// StatusReady: the unified merged graph is available under graphify-out/.
	StatusReady Status = "ready"
	// StatusFailed: the last pipeline run failed. A new mutation may retry.
	StatusFailed Status = "failed"
)

// validTransitions lists the allowed next phases from each phase. It is
// permissive by design: ready and failed can both be reopened by a new
// resource (created→ingesting) or a re-merge (ready→merging).
var validTransitions = map[Status][]Status{
	StatusCreated:   {StatusCreated, StatusIngesting, StatusMerging, StatusReady, StatusFailed},
	StatusIngesting: {StatusIngesting, StatusMerging, StatusReady, StatusFailed, StatusCreated},
	StatusMerging:   {StatusMerging, StatusReady, StatusFailed, StatusIngesting},
	StatusReady:     {StatusReady, StatusIngesting, StatusMerging, StatusFailed},
	StatusFailed:    {StatusFailed, StatusIngesting, StatusMerging, StatusReady, StatusCreated},
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	_, ok := validTransitions[s]
	return ok
}

// CanTransition reports whether a memory may move from → to.
func CanTransition(from, to Status) bool {
	for _, next := range validTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}
