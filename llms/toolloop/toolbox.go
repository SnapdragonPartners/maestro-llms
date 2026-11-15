package toolloop

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// getToolboxName is the reserved name of the loop's toolbox loader
// (ADR-0047).
const getToolboxName = "get_toolbox"

// toolboxes is Run's toolbox state (ADR-0047): which boxes are loaded and
// when each was last used. Run owns it for one run; the loaded set goes
// in through Toolboxes.Loaded and out through Outcome.LoadedToolboxes.
type toolboxes struct {
	loaded    map[int]uint64 // box index -> recency stamp
	core      map[string]bool
	boxes     []Toolbox
	tools     []Tool // Config.Tools, the declaration order of the offered definitions
	loaderDef llms.ToolDefinition
	clock     uint64
	limit     int // unique-tool cap, get_toolbox included; 0 = none
}

// newToolboxes validates cfg.Toolboxes against cfg.Tools and returns the
// initial state, or a *ConfigError. validateConfig has already checked the
// tools themselves (names, executors, duplicates).
//
//nolint:cyclop // the rules are intentionally enumerated, like validateConfig's.
func newToolboxes(cfg *Config, choice llms.ToolChoice) (*toolboxes, error) {
	tb := cfg.Toolboxes
	declared := make(map[string]bool, len(cfg.Tools))
	for i := range cfg.Tools {
		name := cfg.Tools[i].Definition.Name
		if name == getToolboxName {
			return nil, &ConfigError{Reason: fmt.Sprintf("Config.Tools[%d] is named %q, which is reserved for the toolbox loader", i, getToolboxName)}
		}
		declared[name] = true
	}
	core, err := nameSet("Toolboxes.Core", tb.Core, declared)
	if err != nil {
		return nil, err
	}
	if len(tb.Boxes) == 0 {
		return nil, &ConfigError{Reason: "Toolboxes.Boxes must contain at least one toolbox"}
	}
	boxIndex := make(map[string]int, len(tb.Boxes))
	reachable := maps.Clone(core)
	for i := range tb.Boxes {
		b := &tb.Boxes[i]
		if b.Name == "" {
			return nil, &ConfigError{Reason: fmt.Sprintf("Toolboxes.Boxes[%d].Name is empty", i)}
		}
		if _, dup := boxIndex[b.Name]; dup {
			return nil, &ConfigError{Reason: fmt.Sprintf("Toolboxes.Boxes contains duplicate toolbox name %q", b.Name)}
		}
		boxIndex[b.Name] = i
		if len(b.Tools) == 0 {
			return nil, &ConfigError{Reason: fmt.Sprintf("toolbox %q has no tools", b.Name)}
		}
		members, err := nameSet(fmt.Sprintf("toolbox %q", b.Name), b.Tools, declared)
		if err != nil {
			return nil, err
		}
		for n := range members {
			reachable[n] = true
		}
	}
	for i := range cfg.Tools {
		if name := cfg.Tools[i].Definition.Name; !reachable[name] {
			return nil, &ConfigError{Reason: fmt.Sprintf("tool %q is neither core nor in any toolbox, so it could never be offered", name)}
		}
	}

	limit := tb.MaxTools
	switch {
	case limit == 0:
		limit = DefaultMaxTools
	case limit < 0:
		limit = 0
	}
	s := &toolboxes{
		loaded: make(map[int]uint64, len(tb.Loaded)),
		core:   core,
		boxes:  tb.Boxes,
		tools:  cfg.Tools,
		limit:  limit,
	}
	if limit > 0 {
		for i := range tb.Boxes {
			if n := s.countWith(i); n > limit {
				return nil, &ConfigError{Reason: fmt.Sprintf(
					"toolbox %q with the core tools and %s offers %d tools, over the cap of %d; set Toolboxes.MaxTools or shrink the toolbox",
					tb.Boxes[i].Name, getToolboxName, n, limit)}
			}
		}
	}

	if choice.Type == llms.ToolChoiceTool && choice.Name != getToolboxName && !core[choice.Name] {
		return nil, &ConfigError{Reason: fmt.Sprintf(
			"ToolChoice names tool %q, which is not core; with toolboxes a forced tool must be core or %s, since a toolbox tool can be put away", choice.Name, getToolboxName)}
	}

	for _, name := range tb.Loaded {
		i, ok := boxIndex[name]
		if !ok {
			return nil, &ConfigError{Reason: fmt.Sprintf("Toolboxes.Loaded names unknown toolbox %q", name)}
		}
		if _, dup := s.loaded[i]; dup {
			return nil, &ConfigError{Reason: fmt.Sprintf("Toolboxes.Loaded contains duplicate toolbox %q", name)}
		}
		s.clock++
		s.loaded[i] = s.clock
	}
	// Loaded is stored state: the app may have lowered MaxTools since it was
	// saved. Put away the least recently used until it fits; the cap check
	// above guarantees one box always fits.
	for s.overCap() {
		victim := s.leastRecent(-1)
		if victim < 0 || len(s.loaded) == 1 {
			break // unreachable: one box always fits
		}
		delete(s.loaded, victim)
	}
	s.loaderDef = s.loaderDefinition(tb.Guidance)
	return s, nil
}

// nameSet resolves names against the declared tools, rejecting unknown and
// repeated names.
func nameSet(where string, names []string, declared map[string]bool) (map[string]bool, error) {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		if !declared[n] {
			return nil, &ConfigError{Reason: fmt.Sprintf("%s names unknown tool %q", where, n)}
		}
		if set[n] {
			return nil, &ConfigError{Reason: fmt.Sprintf("%s names tool %q twice", where, n)}
		}
		set[n] = true
	}
	return set, nil
}

// countWith is the number of unique tools offered with box i loaded alone:
// core ∪ box, plus get_toolbox.
func (s *toolboxes) countWith(i int) int {
	n := len(s.core) + 1
	for _, t := range s.boxes[i].Tools {
		if !s.core[t] {
			n++
		}
	}
	return n
}

// offered is the set of tool names the next turn offers, without
// get_toolbox: core ∪ every loaded box.
func (s *toolboxes) offered() map[string]bool {
	set := maps.Clone(s.core)
	for i := range s.loaded {
		for _, t := range s.boxes[i].Tools {
			set[t] = true
		}
	}
	return set
}

// overCap reports whether the offered tools, get_toolbox included, exceed
// the cap.
func (s *toolboxes) overCap() bool {
	return s.limit > 0 && len(s.offered()) >= s.limit // offered()+1 > limit
}

// definitions returns the next turn's tool definitions, get_toolbox first
// and then the offered tools in Config.Tools order, and the set of names
// they advertise.
func (s *toolboxes) definitions() ([]llms.ToolDefinition, map[string]bool) {
	set := s.offered()
	defs := make([]llms.ToolDefinition, 0, len(set)+1)
	defs = append(defs, s.loaderDef)
	for i := range s.tools {
		if set[s.tools[i].Definition.Name] {
			defs = append(defs, s.tools[i].Definition)
		}
	}
	return defs, set
}

// older orders loaded boxes by recency, least recent first. A shared-tool
// call stamps several boxes alike, so ties fall back to Boxes order. The
// same order is Outcome.LoadedToolboxes, so it survives into the next run.
func (s *toolboxes) older(a, b int) int {
	if c := compareStamp(s.loaded[a], s.loaded[b]); c != 0 {
		return c
	}
	return a - b
}

// leastRecent returns the least recently used loaded box other than except
// (-1 for none), or -1 if there is none.
func (s *toolboxes) leastRecent(except int) int {
	victim := -1
	for i := range s.loaded {
		if i != except && (victim < 0 || s.older(i, victim) < 0) {
			victim = i
		}
	}
	return victim
}

// used marks every loaded box that supplies tool as used, for a call to a
// tool the turn offered. Core tools need no box, so they mark nothing.
func (s *toolboxes) used(tool string) {
	if s.core[tool] {
		return
	}
	stamped := false
	for i := range s.loaded {
		if slices.Contains(s.boxes[i].Tools, tool) {
			if !stamped {
				s.clock++
				stamped = true
			}
			s.loaded[i] = s.clock
		}
	}
}

// loadedNames returns the loaded boxes, least recently used first.
func (s *toolboxes) loadedNames() []string {
	idx := make([]int, 0, len(s.loaded))
	for i := range s.loaded {
		idx = append(idx, i)
	}
	slices.SortFunc(idx, s.older)
	names := make([]string, len(idx))
	for k, i := range idx {
		names[k] = s.boxes[i].Name
	}
	return names
}

func compareStamp(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func (s *toolboxes) loaderDefinition(guidance string) llms.ToolDefinition {
	var desc strings.Builder
	desc.WriteString("Load a toolbox: a named set of related tools. Its tools become available on your next step. " +
		"Loaded toolboxes stay loaded; if too many tools are loaded, the least recently used toolbox is put away, " +
		"and you can load it again. Toolboxes:")
	names := make([]string, len(s.boxes))
	for i := range s.boxes {
		names[i] = s.boxes[i].Name
		desc.WriteString("\n- " + s.boxes[i].Name)
		if s.boxes[i].Description != "" {
			desc.WriteString(": " + s.boxes[i].Description)
		}
	}
	if guidance != "" {
		desc.WriteString("\n" + guidance)
	}
	schema, _ := json.Marshal(map[string]any{ //nolint:errchkjson // static shape of strings
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "enum": names, "description": "The toolbox to load."},
		},
		"required": []string{"name"},
	})
	return llms.ToolDefinition{Name: getToolboxName, Description: desc.String(), InputSchema: schema}
}

// load executes a get_toolbox call. Every outcome is model-visible: an
// unknown toolbox or unparseable arguments is an IsError result, and the
// loaded set changes only on success.
func (s *toolboxes) load(call llms.ToolCall) ToolResult {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(call.Parameters, &args); err != nil || args.Name == "" {
		return ToolResult{IsError: true, Content: fmt.Sprintf(
			`%s needs {"name": "<toolbox>"}; got %s. Toolboxes: %s.`, getToolboxName, call.Parameters, s.boxList())}
	}
	idx := slices.IndexFunc(s.boxes, func(b Toolbox) bool { return b.Name == args.Name })
	if idx < 0 {
		return ToolResult{IsError: true, Content: fmt.Sprintf("unknown toolbox %q. Toolboxes: %s.", args.Name, s.boxList())}
	}
	box := &s.boxes[idx]
	if _, ok := s.loaded[idx]; ok {
		s.clock++
		s.loaded[idx] = s.clock
		return ToolResult{Content: fmt.Sprintf("Toolbox %q is already loaded; its tools are available: %s.\nLoaded toolboxes: %s.",
			box.Name, strings.Join(box.Tools, ", "), strings.Join(s.loadedNames(), ", "))}
	}

	before := s.offered()
	putAway := s.makeRoom(idx)
	return ToolResult{Content: s.report(box, before, putAway)}
}

// makeRoom loads box idx, putting away the least recently used other boxes
// until the offered tools fit the cap, and stamps idx as the newest. The
// requested box is never put away; the cap check in newToolboxes
// guarantees it fits alone. It returns the boxes put away, in order.
func (s *toolboxes) makeRoom(idx int) []int {
	s.loaded[idx] = s.clock + 1
	var putAway []int
	for s.overCap() {
		victim := s.leastRecent(idx)
		if victim < 0 {
			break // unreachable: the cap check guarantees the box fits alone
		}
		delete(s.loaded, victim)
		putAway = append(putAway, victim)
	}
	s.clock++
	s.loaded[idx] = s.clock
	return putAway
}

// report is get_toolbox's result after loading box: the tools it newly
// offers (with descriptions), the ones already offered, the boxes put away
// and the tools actually no longer offered (a tool another loaded box or
// core still supplies is not listed), and the loaded set.
func (s *toolboxes) report(box *Toolbox, before map[string]bool, putAway []int) string {
	after := s.offered()
	var b strings.Builder
	fmt.Fprintf(&b, "Loaded toolbox %q.", box.Name)
	var already []string
	first := true
	for i := range s.tools {
		t := &s.tools[i].Definition
		switch {
		case !slices.Contains(box.Tools, t.Name):
			continue
		case before[t.Name]:
			already = append(already, t.Name)
			continue
		case first:
			b.WriteString("\nNew tools:")
			first = false
		}
		b.WriteString("\n- " + t.Name)
		if t.Description != "" {
			b.WriteString(": " + t.Description)
		}
	}
	if len(already) > 0 {
		b.WriteString("\nAlready available: " + strings.Join(already, ", ") + ".")
	}
	if len(putAway) > 0 {
		names := make([]string, len(putAway))
		for k, v := range putAway {
			names[k] = fmt.Sprintf("%q", s.boxes[v].Name)
		}
		// Eviction stops only once the count drops, so at least one tool is
		// gone; tools core or a loaded box still supplies are not listed.
		var gone []string
		for i := range s.tools {
			if n := s.tools[i].Definition.Name; before[n] && !after[n] {
				gone = append(gone, n)
			}
		}
		fmt.Fprintf(&b, "\nPut away %s %s to stay within %d tools; no longer available: %s. Call %s to load one again.",
			plural(len(names), "toolbox", "toolboxes"), strings.Join(names, ", "), s.limit, strings.Join(gone, ", "), getToolboxName)
	}
	b.WriteString("\nLoaded toolboxes: " + strings.Join(s.loadedNames(), ", ") + ".")
	return b.String()
}

func (s *toolboxes) boxList() string {
	names := make([]string, len(s.boxes))
	for i := range s.boxes {
		names[i] = s.boxes[i].Name
	}
	return strings.Join(names, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
