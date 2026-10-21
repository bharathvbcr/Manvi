// Package harness holds the pieces of the agent's working environment that
// come from the repository itself rather than from the binary: today, the
// skills a repository ships under .agents/skills.
//
// Everything here is read from the repository, so everything here is untrusted
// text in the same sense AGENTS.md is: it reaches the model's context because
// the repository's owner put it there, and nothing in this package makes it
// more trustworthy than that.
package harness

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/prompt"
)

const (
	// SkillsDir is the only directory skills are read from, relative to the
	// repository root. DevCouncil's `devmap skills install` writes here.
	SkillsDir = ".agents/skills"
	// SkillFile is the one file read out of each skill directory.
	SkillFile = "SKILL.md"

	// MaxSkillBytes bounds one SKILL.md. 64 KiB is roughly sixteen thousand
	// tokens: past that a skill is no longer something to load on demand, it is
	// a document that displaces the task it was loaded for. A larger file is
	// rejected with its size rather than truncated, because half a skill reads
	// as a complete one.
	MaxSkillBytes = 64 * 1024
	// MaxSkills bounds how many skill directories are read. The listing goes
	// into the prompt, and each directory costs a gated, pinned read; an
	// unbounded directory would be an unbounded prompt and an unbounded scan.
	// Directories past the bound are counted in SkillSet.NotExamined.
	MaxSkills = 200
	// MaxRejected bounds how many rejections one load records by name. The rest
	// are counted in SkillSet.RejectedOmitted, so a directory of ten thousand
	// bad names is one number and not ten thousand entries.
	MaxRejected = 50
	// MaxSkillNameLen bounds a skill name. A directory name is at most 255
	// bytes on the filesystems that matter; 64 is far past any real skill and
	// keeps a name echoed into a refusal short.
	MaxSkillNameLen = 64
	// MaxDescriptionRunes bounds one description, which is one line of the
	// always-on listing.
	MaxDescriptionRunes = 1024
)

// skillName is the shape a skill directory name must have. It is also the
// traversal defence for the name the model passes to the loader tool: nothing
// that matches can contain a separator, a dot, or a non-ASCII rune.
var skillName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ValidSkillName reports whether name may name a skill directory.
func ValidSkillName(name string) bool {
	return len(name) <= MaxSkillNameLen && skillName.MatchString(name)
}

const badNameReason = "directory name must match [a-z0-9][a-z0-9_-]* and be at most 64 characters"

// QuoteName renders an untrusted name for a message: quoted, escaped, and cut
// at MaxSkillNameLen runes with its true length, so a refusal never echoes an
// attacker-sized string into the model's context or an operator's terminal.
func QuoteName(name string) string {
	n := utf8.RuneCountInString(name)
	if n <= MaxSkillNameLen {
		return strconv.Quote(name)
	}
	return fmt.Sprintf("%s… (%d runes)", strconv.Quote(string([]rune(name)[:MaxSkillNameLen])), n)
}

// Skill is one accepted skill.
type Skill struct {
	Name        string
	Description string
	// Body is the SKILL.md text after the frontmatter.
	Body string
}

// Rejection is a skill directory that was found and refused, and why. A
// rejected skill is reported, never skipped: an operator whose skill does not
// appear needs to be told it was seen. Name is quoted when the directory name
// is not itself a valid skill name.
type Rejection struct {
	Name   string
	Reason string
}

// SkillSet is the outcome of one load. Skills and Rejected are both sorted by
// directory name, so two loads of an unchanged tree are identical.
type SkillSet struct {
	Skills   []Skill
	Rejected []Rejection
	// RejectedOmitted counts rejections past MaxRejected that are not in
	// Rejected.
	RejectedOmitted int
	// NotExamined counts validly named directories past MaxSkills that were
	// never read. A nonzero value means the listing is a prefix, not the whole.
	NotExamined int
}

// Find returns the accepted skill called name.
func (s SkillSet) Find(name string) (Skill, bool) {
	i := sort.Search(len(s.Skills), func(i int) bool { return s.Skills[i].Name >= name })
	if i < len(s.Skills) && s.Skills[i].Name == name {
		return s.Skills[i], true
	}
	return Skill{}, false
}

// Rejection returns the reason the directory called name was refused.
func (s SkillSet) Rejection(name string) (string, bool) {
	for _, r := range s.Rejected {
		if r.Name == name {
			return r.Reason, true
		}
	}
	return "", false
}

// ReadFunc reads one repository-relative file, refusing it when it is larger
// than limit bytes. The caller supplies the repository's contained reader
// (devcouncil.ReadPinned: no symlink escape, regular files only, descriptor
// size check, read gate) so skills are read by the same code as every other
// repository byte. It is injected because that reader lives in a package that
// imports this one. The loader does not trust it to enforce limit.
type ReadFunc func(rel string, limit int64) ([]byte, error)

// LoadSkills reads the skills under <root>/.agents/skills, at most MaxSkills of
// them.
//
// A missing skills directory is "no skills" and not an error; one that exists
// and cannot be listed is an error, because an empty list and "could not look"
// must not be the same answer. Entries that are neither directories nor
// symbolic links (a README, say) are not skills and are left alone; a symbolic
// link is judged by the reader, which refuses one that leaves the repository.
// A name that cannot be a skill costs no read.
func LoadSkills(ctx context.Context, root string, read ReadFunc) (SkillSet, error) {
	names, err := listSkillDirs(ctx, root)
	if err != nil {
		return SkillSet{}, err
	}
	var set SkillSet
	examined := 0
	for _, name := range names {
		if !ValidSkillName(name) {
			set.reject(name, badNameReason)
			continue
		}
		if examined >= MaxSkills {
			set.NotExamined++
			continue
		}
		examined++
		skill, reason, cancelled := readSkill(name, read)
		if cancelled != nil {
			// The read was interrupted, not refused: blaming each remaining
			// skill for it would tell the operator every one is malformed.
			return SkillSet{}, cancelled
		}
		if reason != "" {
			set.reject(name, reason)
			continue
		}
		set.Skills = append(set.Skills, skill)
	}
	return set, nil
}

func (s *SkillSet) reject(name, reason string) {
	if len(s.Rejected) >= MaxRejected {
		s.RejectedOmitted++
		return
	}
	if !ValidSkillName(name) {
		name = QuoteName(name)
	}
	s.Rejected = append(s.Rejected, Rejection{Name: name, Reason: reason})
}

// Outcome is how a single lookup ended.
type Outcome int

const (
	// SkillFound: the skill is in Lookup.Skill.
	SkillFound Outcome = iota
	// SkillMissing: no directory has that name, in any letter case.
	SkillMissing
	// SkillRejected: a directory exists and was refused for Lookup.Reason.
	SkillRejected
)

// Lookup is the answer to a request for one skill by name.
type Lookup struct {
	Outcome Outcome
	Skill   Skill
	// Dir is the directory that answered: Name itself, or the directory that
	// matches it in another letter case.
	Dir    string
	Reason string
	// Available names up to MaxListedNames valid skill directories, for a
	// refusal that tells the caller what exists; AvailableTotal counts them all.
	Available      []string
	AvailableTotal int
	// Directories counts every valid skill directory, listed elsewhere or not,
	// so a refusal can tell "no skills" from "every skill is already listed".
	Directories int
}

// MaxListedNames bounds the names a refusal lists.
const MaxListedNames = 20

// LookupSkill reads and validates only <root>/.agents/skills/<name>/SKILL.md.
// It lists the directory's names (no reads) to tell "absent" from "rejected"
// and to find a directory that differs from name only in letter case, which a
// case-sensitive filesystem would otherwise report as absent. name must pass
// ValidSkillName; the caller refuses anything else before the filesystem is
// involved.
func LookupSkill(ctx context.Context, root string, read ReadFunc, name string) (Lookup, error) {
	return LookupSkillExcluding(ctx, root, read, name, nil)
}

// LookupSkillExcluding is LookupSkill whose refusal of an unknown name offers
// only skills not in listed: the names the prompt already shows are no help to
// a model that asked for one it could not see.
func LookupSkillExcluding(ctx context.Context, root string, read ReadFunc, name string, listed map[string]bool) (Lookup, error) {
	if !ValidSkillName(name) {
		return Lookup{}, fmt.Errorf("%s is not a valid skill name", QuoteName(name))
	}
	names, err := listSkillDirs(ctx, root)
	if err != nil {
		return Lookup{}, err
	}
	dir := ""
	for _, n := range names {
		if n == name {
			dir = n
			break
		}
		if dir == "" && strings.EqualFold(n, name) {
			dir = n
		}
	}
	if dir == "" {
		l := Lookup{Outcome: SkillMissing}
		for _, n := range names {
			if !ValidSkillName(n) {
				continue
			}
			l.Directories++
			if !listed[n] {
				l.AvailableTotal++
				if len(l.Available) < MaxListedNames {
					l.Available = append(l.Available, n)
				}
			}
		}
		return l, nil
	}
	if !ValidSkillName(dir) {
		return Lookup{Outcome: SkillRejected, Dir: dir, Reason: badNameReason}, nil
	}
	skill, reason, cancelled := readSkill(dir, read)
	if cancelled != nil {
		return Lookup{}, cancelled
	}
	if reason != "" {
		return Lookup{Outcome: SkillRejected, Dir: dir, Reason: reason}, nil
	}
	return Lookup{Outcome: SkillFound, Skill: skill, Dir: dir}, nil
}

// listSkillDirs returns the sorted names of candidate skill entries. The
// listing goes through os.Root, which refuses a skills directory that is a
// symbolic link out of the repository.
//
// The path is judged before it is opened: a FIFO, socket, device or plain file
// at .agents/skills is refused as a listing error, because opening a FIFO
// blocks until a writer appears and a repository can plant one. The open and
// the read are also bounded by ctx, since the judgement and the open are two
// steps and a hung filesystem has no type to judge; a cancelled or timed-out
// listing takes the same error path as any other listing failure.
func listSkillDirs(ctx context.Context, root string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("listing %s: %w", SkillsDir, err)
	}
	type outcome struct {
		names []string
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		names, err := readSkillDirs(root)
		done <- outcome{names, err}
	}()
	select {
	case o := <-done:
		return o.names, o.err
	case <-ctx.Done():
		return nil, fmt.Errorf("listing %s: %w", SkillsDir, ctx.Err())
	}
}

func readSkillDirs(root string) ([]string, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("opening repository root: %w", err)
	}
	defer func() { _ = r.Close() }()
	info, err := r.Stat(SkillsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("examining %s: %w", SkillsDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory (%s), so it is not opened", SkillsDir, info.Mode().Type())
	}
	dir, err := r.Open(SkillsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("opening %s: %w", SkillsDir, err)
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", SkillsDir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// loadSkill judges one directory. It returns a reason, never a partial skill.
func loadSkill(dirName string, read ReadFunc) (Skill, string) {
	skill, reason, _ := readSkill(dirName, read)
	return skill, reason
}

// readSkill judges one directory. The error is non-nil only when the read was
// cancelled or timed out, which says nothing about the skill.
func readSkill(dirName string, read ReadFunc) (Skill, string, error) {
	if !ValidSkillName(dirName) {
		return Skill{}, badNameReason, nil
	}
	data, err := read(path.Join(SkillsDir, dirName, SkillFile), MaxSkillBytes)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Skill{}, "", err
		}
		return Skill{}, fmt.Sprintf("cannot read %s: %v", SkillFile, err), nil
	}
	skill, reason := judgeSkill(dirName, data)
	return skill, reason, nil
}

// judgeSkill validates the bytes of one SKILL.md. It returns a reason, never a
// partial skill.
func judgeSkill(dirName string, data []byte) (Skill, string) {
	if len(data) > MaxSkillBytes {
		return Skill{}, fmt.Sprintf("%s is %d bytes, over the %d limit", SkillFile, len(data), MaxSkillBytes)
	}
	if !utf8.Valid(data) {
		return Skill{}, SkillFile + " is not valid UTF-8"
	}
	fields, body, err := ParseFrontmatter(string(data))
	if err != nil {
		return Skill{}, err.Error()
	}
	name, desc := fields["name"], fields["description"]
	if name != dirName {
		if name == "" {
			return Skill{}, "frontmatter has no name"
		}
		return Skill{}, fmt.Sprintf("frontmatter name %s does not match directory %q", QuoteName(name), dirName)
	}
	if desc == "" {
		return Skill{}, "frontmatter has no description"
	}
	if n := utf8.RuneCountInString(desc); n > MaxDescriptionRunes {
		return Skill{}, fmt.Sprintf("description is %d runes, over the %d limit", n, MaxDescriptionRunes)
	}
	return Skill{Name: dirName, Description: desc, Body: body}, ""
}

// unrenderable reports a control, format or private-use rune: one that would
// reach the model and the terminal unrendered. Skills and harness specs share
// this one definition of text that is refused.
func unrenderable(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r)
}

// ParseFrontmatter reads the leading "---" block of a SKILL.md and returns its
// name and description (the only keys anyone here uses) and the text after it.
//
// It is deliberately not a YAML parser. Each value is one line, bare or in
// single or double quotes, and is followed by nothing but whitespace after the
// closing quote. A key is the name or description at the start of a line,
// optionally spaced before its colon, and the colon must be followed by a
// space or the end of the line (`description:x` is not a mapping in YAML). A
// repeated key is refused rather than resolved, because last-wins would let a
// second line replace the one a reviewer read. A block scalar (| or >) is
// refused, because accepting its indicator as the text would put ">" in the
// prompt as the description. A value carrying a control, format or private-use
// rune is refused: it would reach the model and the terminal unrendered. Other
// keys and indented lines are ignored. CRLF is accepted.
func ParseFrontmatter(text string) (map[string]string, string, error) {
	text = strings.TrimPrefix(text, "\ufeff")
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if strings.TrimRight(lines[0], " \t") != "---" {
		return nil, "", errors.New("no frontmatter: the file must start with a --- block")
	}
	fields := map[string]string{}
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimRight(line, " \t") == "---" {
			return fields, strings.TrimLeft(strings.Join(lines[i+1:], "\n"), "\n"), nil
		}
		key, val, ok := strings.Cut(line, ":")
		key = strings.TrimRight(key, " \t")
		if !ok || (key != "name" && key != "description") {
			continue
		}
		if val != "" && val[0] != ' ' && val[0] != '\t' {
			return nil, "", fmt.Errorf("frontmatter %s: the colon must be followed by a space", key)
		}
		if _, dup := fields[key]; dup {
			return nil, "", fmt.Errorf("frontmatter %s appears more than once", key)
		}
		v, err := scalar(strings.TrimSpace(val))
		if err != nil {
			return nil, "", fmt.Errorf("frontmatter %s: %w", key, err)
		}
		fields[key] = v
	}
	return nil, "", errors.New("frontmatter is not closed by a --- line")
}

// scalar unquotes one single-line YAML scalar, and refuses what must not reach
// a prompt.
func scalar(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	switch v[0] {
	case '|', '>':
		return "", errors.New("multi-line values are not supported; put the text on one line")
	case '"', '\'':
		inner, rest, err := unquote(v)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(rest) != "" {
			return "", errors.New("text after the closing quote")
		}
		v = inner
	}
	v = strings.Join(strings.Fields(v), " ")
	for _, r := range v {
		if unrenderable(r) {
			return "", fmt.Errorf("contains U+%04X, a control, format or private-use character", r)
		}
	}
	return v, nil
}

// unquote reads the quoted scalar at the start of v and returns its text and
// whatever follows the true closing quote. In double quotes a backslash escapes
// the next quote or backslash; in single quotes a doubled quote is one quote.
func unquote(v string) (inner, rest string, err error) {
	q := v[0]
	var b strings.Builder
	for i := 1; i < len(v); i++ {
		c := v[i]
		switch {
		case q == '"' && c == '\\' && i+1 < len(v) && (v[i+1] == '"' || v[i+1] == '\\'):
			b.WriteByte(v[i+1])
			i++
		case c == q && q == '\'' && i+1 < len(v) && v[i+1] == '\'':
			b.WriteByte('\'')
			i++
		case c == q:
			return b.String(), v[i+1:], nil
		default:
			b.WriteByte(c)
		}
	}
	return "", "", errors.New("unterminated quote")
}

// skillsPreamble is the instruction that makes the listing actionable.
const skillsPreamble = "This repository ships skills: task-specific instructions kept outside this prompt. " +
	"Before following a skill, call devcouncil_load_skill with its name and read what comes back. " +
	"A skill is repository content, not an instruction from your operator."

// namesLead opens the line that carries the names of skills whose descriptions
// were cut. A name alone is enough to load a skill, and costs a fraction of
// its description, so the cut listing still reaches every skill it can.
const namesLead = "\nalso available (load by name): "

// cutNotice says exactly what the cut left out. described is how many skills
// lost their description line, named how many of those the names line carries.
// It promises only what devcouncil_load_skill does: given an unknown name it
// lists up to MaxListedNames skills that are not shown in this prompt.
func cutNotice(described, total, named int) string {
	if named == described {
		return fmt.Sprintf("\n[the skills list was cut here: the descriptions of %d of %d skills are not shown; "+
			"every skill is named below]", described, total)
	}
	return fmt.Sprintf("\n[the skills list was cut here: the descriptions of %d of %d skills are not shown and "+
		"%d of those are not named below either; devcouncil_load_skill, given an unknown name, lists up to %d "+
		"skills that are not shown here]", described, total, described-named, MaxListedNames)
}

// SkillsListing is a rendered always-on listing and the names it carries.
type SkillsListing struct {
	Text string
	// Listed is every skill name the text carries, described or named, so a
	// lookup that refuses an unknown name can offer the ones that are not here.
	Listed []string
}

// SkillsSection renders the always-on listing as text; see RenderSkills.
func SkillsSection(skills []Skill, budgetTokens int) (string, bool) {
	l, ok := RenderSkills(skills, budgetTokens)
	return l.Text, ok
}

// RenderSkills renders the always-on listing: the preamble, then one
// "name — description" line per skill, sorted by name. It reports false when
// there is nothing to list, so a repository without skills gets no section.
//
// budgetTokens is measured with prompt.EstimateTokens, the unit the rest of the
// prompt's budget is in, and bounds the whole text; zero or less means
// unbounded, as for a provider with no prompt ceiling. A listing over budget
// keeps as many whole description lines as leave room for a cut notice and the
// names of every skill that lost its description; when even that does not fit
// it names as many as do, and the notice says how many are named nowhere. When
// not even the preamble and a notice fit there is no section: a listing that
// cannot say it is incomplete must not appear. The result depends only on its
// arguments, so two calls are byte-identical, which the prompt-cache prefix
// needs.
func RenderSkills(skills []Skill, budgetTokens int) (SkillsListing, bool) {
	if len(skills) == 0 {
		return SkillsListing{}, false
	}
	sorted := append([]Skill(nil), skills...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	n := len(sorted)
	names := make([]string, n)
	lines := make([]string, n)
	for i, s := range sorted {
		names[i] = s.Name
		lines[i] = "\n- " + s.Name + " — " + s.Description
	}
	tail := func(k, m int) string {
		if k >= n {
			return ""
		}
		t := cutNotice(n-k, n, m)
		if m > 0 {
			t += namesLead + strings.Join(names[k:k+m], ", ")
		}
		return t
	}
	build := func(k, m int) SkillsListing {
		var b strings.Builder
		b.WriteString(skillsPreamble)
		b.WriteString("\n")
		for _, l := range lines[:k] {
			b.WriteString(l)
		}
		b.WriteString(tail(k, m))
		return SkillsListing{Text: b.String(), Listed: append([]string(nil), names[:k+m]...)}
	}
	if budgetTokens <= 0 {
		return build(n, 0), true
	}
	// prefix[k] is the estimated cost of the head and the first k lines, summed
	// per line. It is a filter that stops growing once it passes the budget, so
	// a repository with hundreds of skills is not estimated in full every turn;
	// a candidate is accepted only on the estimate of its whole text.
	prefix := []int{prompt.EstimateTokens(skillsPreamble + "\n")}
	for k := 0; k < n && prefix[k] <= budgetTokens; k++ {
		prefix = append(prefix, prefix[k]+prompt.EstimateTokens(lines[k]))
	}
	try := func(k, m int) (SkillsListing, bool) {
		if k >= len(prefix) || prefix[k]+prompt.EstimateTokens(tail(k, m)) > budgetTokens {
			return SkillsListing{}, false
		}
		cand := build(k, m)
		return cand, prompt.EstimateTokens(cand.Text) <= budgetTokens
	}
	if cand, ok := try(n, 0); ok {
		return cand, true
	}
	// Most descriptions first, each still naming everything it cut.
	for k := min(n-1, len(prefix)-1); k >= 0; k-- {
		if cand, ok := try(k, n-k); ok {
			return cand, true
		}
	}
	// Not even every name fits: no descriptions, as many names as fit.
	for m := n - 1; m >= 0; m-- {
		if cand, ok := try(0, m); ok {
			return cand, true
		}
	}
	return SkillsListing{}, false
}
