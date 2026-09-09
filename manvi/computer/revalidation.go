package computer

import (
	"crypto/sha256"
	"encoding/json"
	"sort"
)

type approvalChanged struct{}

func (*approvalChanged) Error() string { return "application state changed during approval" }

// This digest stays in memory. Including protected values detects changes that
// masked screenshots cannot show; persisting a plain hash would expose a guessing
// oracle for short private identifiers. Pixel positions, focus and capture IDs
// are excluded. The broker revalidates geometry and actionability at dispatch.
func semanticFingerprint(o Observation) [32]byte {
	type semanticNode struct {
		Role, Name, Identifier, Value string
		HasValue, Enabled, Editable   bool
		Path                          []uint32
		Actions                       []string
	}
	nodes := make([]string, 0, len(o.Nodes))
	for _, n := range o.Nodes {
		v := semanticNode{Role: n.Role, Name: n.Name, Enabled: n.Enabled, Editable: n.Editable, Path: n.NativePath, Actions: append([]string(nil), n.Actions...)}
		if n.Identifier != nil {
			v.Identifier = *n.Identifier
		}
		if n.Value != nil {
			v.HasValue = true
			v.Value = *n.Value
		}
		sort.Strings(v.Actions)
		raw, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		nodes = append(nodes, string(raw))
	}
	sort.Strings(nodes)
	raw, err := json.Marshal(struct {
		PID            uint32
		ID             uint64
		Process, Title string
		Nodes          []string
	}{o.Window.PID, o.Window.ID, o.Window.ProcessIdentity, o.Window.Title, nodes})
	if err != nil {
		panic(err)
	}
	return sha256.Sum256(raw)
}
