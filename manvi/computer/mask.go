package computer

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"image"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// The folded secret matcher is the one owner of "does this text carry a watched
// secret". The literal match it replaces is defeated by any application that
// reformats what it was given ("4111 1111 1111 1111", full-width digits, a
// non-breaking hyphen, zero-width joiners), so both the secret and the text are
// folded before they are compared: full-width forms become ASCII, letters lose
// their case, and separators (spaces of every kind, dash variants, dots, commas,
// slashes, connector punctuation and format controls such as zero-width joiners)
// are dropped. Only Unicode characters the standard library can fold without a
// table are folded; other scripts' digits are not.
const (
	// minFoldedSecretLen is the shortest folded secret that is matched after
	// folding. A shorter one (a 4-digit PIN, a one-letter value) would blank every
	// unrelated digit run once separators are dropped, so it keeps only the
	// literal match it has always had.
	minFoldedSecretLen = 6
	// maxSplitSiblings and maxSplitBytes bound the search for a secret divided
	// over adjacent siblings of one parent: a window never spans more nodes, or
	// more folded bytes, than this.
	maxSplitSiblings = 8
	maxSplitBytes    = 4096
	// maxRememberedMasks bounds the masked-control bounds a run remembers; past it
	// the oldest is forgotten first.
	maxRememberedMasks = 256
	// rememberedOverlap is the share of a node's own area that must lie inside a
	// remembered mask for it to count as the same control re-created.
	rememberedOverlap = 0.5
)

const redactedText = "[redacted]"

func foldSensitive(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		if unicode.IsSpace(r) || unicode.In(r, unicode.Cf, unicode.Z, unicode.Pd, unicode.Pc) || strings.ContainsRune(".,/\\·•", r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// secretMatcher is the watched secrets in both forms.
type secretMatcher struct {
	literal []string
	folded  []string
}

func (p PrivacyPolicy) matcher() secretMatcher {
	m := secretMatcher{}
	for _, secret := range p.Watched {
		if secret == "" {
			continue
		}
		m.literal = append(m.literal, secret)
		if f := foldSensitive(secret); utf8.RuneCountInString(f) >= minFoldedSecretLen {
			m.folded = append(m.folded, f)
		}
	}
	return m
}

// carries reports whether s holds a watched secret, literally or folded.
func (m secretMatcher) carries(s string) bool {
	for _, secret := range m.literal {
		if strings.Contains(s, secret) {
			return true
		}
	}
	return m.foldedCarries(s)
}

func (m secretMatcher) foldedCarries(s string) bool {
	if len(m.folded) == 0 || s == "" {
		return false
	}
	f := foldSensitive(s)
	for _, secret := range m.folded {
		if strings.Contains(f, secret) {
			return true
		}
	}
	return false
}

// redacted marks the fields of a node whose text joined a secret split over
// several nodes.
type redacted struct{ name, value, identifier bool }

// maskPlan is what a frame's nodes must hide: sensitive[i] masks node i's value
// and pixels, hide[i] also redacts the fields that carried part of a split secret.
type maskPlan struct {
	sensitive []bool
	hide      []redacted
}

func nodeField(n Node, field int) string {
	switch field {
	case 0:
		return n.Name
	case 1:
		if n.Value != nil {
			return *n.Value
		}
	default:
		if n.Identifier != nil {
			return *n.Identifier
		}
	}
	return ""
}

// plan decides, for one frame, every node that must be masked: the control a
// sensitive parameter was written into (by role, name and identifier), any node
// whose text carries a watched secret (folded), any node holding a part of a
// secret split over adjacent siblings of one parent, and any node lying mostly
// where a masked control lay earlier in the run (the application re-created it
// under a new identifier or name). It only ever adds nodes to what the literal
// match alone would have masked.
func (p PrivacyPolicy) plan(nodes []Node) maskPlan {
	m := p.matcher()
	out := maskPlan{sensitive: make([]bool, len(nodes)), hide: make([]redacted, len(nodes))}
	for i, n := range nodes {
		out.sensitive[i] = p.sensitiveWith(m, n)
	}
	m.markSplit(nodes, &out)
	if p.memory != nil {
		p.memory.overlap(nodes, &out)
		p.memory.remember(nodes, out.sensitive)
	}
	return out
}

func (p PrivacyPolicy) sensitive(n Node) bool { return p.sensitiveWith(p.matcher(), n) }

func (p PrivacyPolicy) sensitiveWith(m secretMatcher, n Node) bool {
	for _, target := range p.SensitiveTargets {
		if (target.Role == "" || target.Role == n.Role) && (target.Name == "" || target.Name == n.Name) && (target.Identifier == "" || (n.Identifier != nil && target.Identifier == *n.Identifier)) {
			return true
		}
	}
	for field := 0; field < 3; field++ {
		if m.carries(nodeField(n, field)) {
			return true
		}
	}
	return false
}

// markSplit finds a folded secret that spans two or more consecutive nodes of
// one parent (nodes without a parent form one group), per field, over windows of
// at most maxSplitSiblings nodes and maxSplitBytes folded bytes.
func (m secretMatcher) markSplit(nodes []Node, out *maskPlan) {
	if len(m.folded) == 0 {
		return
	}
	groups := map[string][]int{}
	var order []string
	for i, n := range nodes {
		key := ""
		if n.Parent != nil {
			key = *n.Parent
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}
	for _, key := range order {
		idx := groups[key]
		for field := 0; field < 3; field++ {
			texts := make([]string, len(idx))
			for k, i := range idx {
				texts[k] = foldSensitive(nodeField(nodes[i], field))
			}
			for start := range idx {
				if texts[start] == "" {
					continue
				}
				var joined strings.Builder
				var ends []int
				for k := start; k < len(idx) && k-start < maxSplitSiblings && joined.Len()+len(texts[k]) <= maxSplitBytes; k++ {
					joined.WriteString(texts[k])
					ends = append(ends, joined.Len())
				}
				if len(ends) < 2 {
					continue
				}
				text := joined.String()
				for _, secret := range m.folded {
					at := strings.Index(text, secret)
					if at < 0 {
						continue
					}
					lo, hi := at, at+len(secret)
					segStart := 0
					for k, end := range ends {
						if segStart < hi && end > lo && end > segStart {
							i := idx[start+k]
							out.sensitive[i] = true
							switch field {
							case 0:
								out.hide[i].name = true
							case 1:
								out.hide[i].value = true
							default:
								out.hide[i].identifier = true
							}
						}
						segStart = end
					}
				}
			}
		}
	}
}

// maskMemory is the run-scoped record of where masked controls lay, shared by
// every copy of the policy a run holds.
type maskMemory struct {
	mu     sync.Mutex
	bounds []Bounds
}

func validBounds(b *Bounds) bool {
	return b != nil && finite(b.X) && finite(b.Y) && finite(b.Width) && finite(b.Height) && b.Width > 0 && b.Height > 0
}

// overlapShare is the share of a that lies inside b.
func overlapShare(a, b Bounds) float64 {
	w := min(a.X+a.Width, b.X+b.Width) - max(a.X, b.X)
	h := min(a.Y+a.Height, b.Y+b.Height) - max(a.Y, b.Y)
	if w <= 0 || h <= 0 {
		return 0
	}
	return (w * h) / (a.Width * a.Height)
}

func (m *maskMemory) overlap(nodes []Node, out *maskPlan) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, n := range nodes {
		if out.sensitive[i] || !validBounds(n.Bounds) {
			continue
		}
		for _, b := range m.bounds {
			if overlapShare(*n.Bounds, b) > rememberedOverlap {
				out.sensitive[i] = true
				break
			}
		}
	}
}

func (m *maskMemory) remember(nodes []Node, sensitive []bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, n := range nodes {
		if !sensitive[i] || !validBounds(n.Bounds) {
			continue
		}
		known := false
		for _, b := range m.bounds {
			if b == *n.Bounds {
				known = true
				break
			}
		}
		if known {
			continue
		}
		if len(m.bounds) >= maxRememberedMasks {
			m.bounds = m.bounds[1:]
		}
		m.bounds = append(m.bounds, *n.Bounds)
	}
}

// bindMasks folds where the masks landed into a frame digest, so two frames that
// differ only in which pixels were hidden do not share an approval digest. With no
// mask the digest is returned unchanged.
func bindMasks(digest [32]byte, masks []image.Rectangle) [32]byte {
	if len(masks) == 0 {
		return digest
	}
	h := sha256.New()
	h.Write(digest[:])
	// Each coordinate is written as a signed varint: injective for every int
	// (a negative or oversized coordinate cannot collide with another), and with
	// no narrowing conversion to reason about.
	var buf [binary.MaxVarintLen64]byte
	for _, r := range masks {
		for _, v := range [4]int{r.Min.X, r.Min.Y, r.Max.X, r.Max.Y} {
			n := binary.PutVarint(buf[:], int64(v))
			h.Write(buf[:n])
		}
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// describeNode names a node by role, name and identifier for an error message,
// scrubbed and truncated. It never includes the node's value.
func (p PrivacyPolicy) describeNode(n Node) string {
	clip := func(s string) string {
		s = p.Scrub(s)
		if r := []rune(s); len(r) > 48 {
			s = string(r[:48]) + "..."
		}
		return s
	}
	id := ""
	if n.Identifier != nil {
		id = *n.Identifier
	}
	return fmt.Sprintf("%q (role %q, identifier %q)", clip(n.Name), clip(n.Role), clip(id))
}
