package memory

import (
	"encoding/json"
	"path"
	"regexp"
	"sort"
	"strings"
)

// CorrelateResult summarizes what a correlation pass added.
type CorrelateResult struct {
	HubsAdded  int
	LinksAdded int
}

// Correlate is a best-effort, deterministic post-merge pass over a merged
// graphify graph (networkx node-link JSON). graphify's own `merge-graphs`
// already namespaces node ids per source and resolves cross-repo calls by exact
// label match; this pass adds an explicit, generic correlation layer on top:
// for every entity name that appears in two or more distinct sources, it emits
// a single hub node (`hub::entity::<name>`) and links each occurrence to it
// with an INFERRED "correlates" edge.
//
// The result is that a user querying the unified memory can pivot on shared
// concepts (a module, class, or function present in multiple repos/files)
// without the caller knowing the per-source namespacing.
//
// It is intentionally conservative and schema-defensive: on any input it does
// not recognize (not a JSON object, missing/!array nodes or links, nodes
// without string ids) it returns the original bytes unchanged with a zero
// result. It is idempotent — re-running skips hub nodes it already created.
//
// NOTE: domain-specific CI/CD correlation (Vionix DevSecOps: image↔service↔repo
// hub nodes) is deliberately out of scope here and deferred to Phase 2.
func Correlate(raw []byte) ([]byte, CorrelateResult, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw, CorrelateResult{}, nil // not an object we understand → no-op
	}

	nodesAny, ok := doc["nodes"].([]any)
	if !ok {
		return raw, CorrelateResult{}, nil
	}
	linksKey := "links"
	linksAny, ok := doc["links"].([]any)
	if !ok {
		if e, ok2 := doc["edges"].([]any); ok2 {
			linksAny, linksKey = e, "edges"
		} else {
			return raw, CorrelateResult{}, nil
		}
	}

	// Collect existing hub ids so re-runs are idempotent.
	existingHubs := map[string]bool{}
	for _, n := range nodesAny {
		if m, ok := n.(map[string]any); ok {
			if id, _ := m["id"].(string); strings.HasPrefix(id, hubPrefix) {
				existingHubs[id] = true
			}
		}
	}

	// Group node ids by correlation key, tracking the distinct sources each key
	// spans. Only keys spanning ≥2 sources become hubs.
	type group struct {
		ids     []string
		sources map[string]bool
	}
	groups := map[string]*group{}
	for _, n := range nodesAny {
		m, ok := n.(map[string]any)
		if !ok {
			continue
		}
		id, ok := m["id"].(string)
		if !ok || id == "" || strings.HasPrefix(id, hubPrefix) {
			continue
		}
		key := correlationKey(m)
		if key == "" {
			continue
		}
		src := sourceKey(m)
		if src == "" {
			continue
		}
		g := groups[key]
		if g == nil {
			g = &group{sources: map[string]bool{}}
			groups[key] = g
		}
		g.ids = append(g.ids, id)
		g.sources[src] = true
	}

	// Deterministic order: sort keys so output is reproducible.
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var res CorrelateResult
	for _, key := range keys {
		g := groups[key]
		if len(g.sources) < 2 {
			continue // present in only one source — nothing to correlate
		}
		hubID := hubPrefix + key
		if !existingHubs[hubID] {
			nodesAny = append(nodesAny, map[string]any{
				"id":         hubID,
				"label":      key,
				"file_type":  "hub",
				"node_type":  "hub",
				"correlated": true,
			})
			existingHubs[hubID] = true
			res.HubsAdded++
		}
		sort.Strings(g.ids)
		for _, id := range g.ids {
			linksAny = append(linksAny, map[string]any{
				"source":     hubID,
				"target":     id,
				"_src":       hubID,
				"_tgt":       id,
				"relation":   "correlates",
				"confidence": "INFERRED",
				"weight":     1.0,
			})
			res.LinksAdded++
		}
	}

	if res.HubsAdded == 0 && res.LinksAdded == 0 {
		return raw, res, nil // no cross-source overlap → leave original untouched
	}

	doc["nodes"] = nodesAny
	doc[linksKey] = linksAny
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return raw, CorrelateResult{}, err
	}
	return out, res, nil
}

const hubPrefix = "hub::entity::"

// methodLabel matches bound-method labels like ".__init__()" or ".forward()"
// which are ubiquitous and would create meaningless mega-hubs if correlated.
var methodLabel = regexp.MustCompile(`^\.`)

// correlationKey derives a normalized, comparable entity name for a node, or ""
// if the node should not participate in correlation. It only correlates named,
// code-level entities (files, classes, top-level functions) and skips bound
// methods and unlabeled nodes to keep hubs meaningful.
func correlationKey(m map[string]any) string {
	label, _ := m["label"].(string)
	label = strings.TrimSpace(label)
	if len(label) < 2 || methodLabel.MatchString(label) {
		return ""
	}
	if ft, ok := m["file_type"].(string); ok && ft != "" && ft != "code" {
		// Non-code nodes (e.g. hub) are excluded; code files/symbols included.
		return ""
	}
	return strings.ToLower(label)
}

// sourceKey identifies which source graph a node came from, so correlation only
// links entities across *different* sources. Prefers explicit tags emitted by a
// merge pass, falling back to the top two segments of source_file.
func sourceKey(m map[string]any) string {
	// `repo` is the attribute graphify's merge-graphs stamps on every node.
	for _, k := range []string{"repo", "source_repo", "source_graph"} {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	if sf, ok := m["source_file"].(string); ok && sf != "" {
		clean := strings.TrimPrefix(path.Clean(sf), "/")
		parts := strings.SplitN(clean, "/", 3)
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
		return parts[0]
	}
	return ""
}
