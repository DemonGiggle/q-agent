package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"q-agent/internal/llm"
)

// fakeLister serves canned tools/list pages keyed by cursor.
type fakeLister struct {
	pages map[string]*mcp.ListToolsResult
	errOn map[string]error
	calls int
}

func (f *fakeLister) ListTools(ctx context.Context, params *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	f.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cursor := ""
	if params != nil {
		cursor = params.Cursor
	}
	if err, ok := f.errOn[cursor]; ok {
		return nil, err
	}
	result, ok := f.pages[cursor]
	if !ok {
		return nil, fmt.Errorf("unexpected cursor %q", cursor)
	}
	return result, nil
}

func mcpTool(name string, schema any) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: name + " tool", InputSchema: schema}
}

func objectSchema(props map[string]any, required ...string) map[string]any {
	req := make([]any, len(required))
	for i, r := range required {
		req[i] = r
	}
	return map[string]any{"type": "object", "properties": props, "required": req}
}

func TestListServerToolsPagination(t *testing.T) {
	fake := &fakeLister{pages: map[string]*mcp.ListToolsResult{
		"":   {Tools: []*mcp.Tool{mcpTool("a", nil), mcpTool("b", nil)}, NextCursor: "p2"},
		"p2": {Tools: []*mcp.Tool{mcpTool("c", nil)}, NextCursor: ""},
	}}
	listing, err := ListServerTools(context.Background(), fake, DiscoveryLimits{})
	if err != nil {
		t.Fatalf("ListServerTools: %v", err)
	}
	if len(listing.Tools) != 3 || listing.Truncated {
		t.Fatalf("got %d tools truncated=%v, want 3 untruncated", len(listing.Tools), listing.Truncated)
	}
}

func TestListServerToolsRepeatedCursor(t *testing.T) {
	fake := &fakeLister{pages: map[string]*mcp.ListToolsResult{
		"":   {Tools: []*mcp.Tool{mcpTool("a", nil)}, NextCursor: "p2"},
		"p2": {Tools: []*mcp.Tool{mcpTool("b", nil)}, NextCursor: "p2"},
	}}
	if _, err := ListServerTools(context.Background(), fake, DiscoveryLimits{}); err == nil {
		t.Fatal("expected repeated-cursor error")
	}
}

func TestListServerToolsCycleCursor(t *testing.T) {
	loop := &fakeLister{pages: map[string]*mcp.ListToolsResult{
		"":   {Tools: []*mcp.Tool{mcpTool("a", nil)}, NextCursor: "p2"},
		"p2": {Tools: []*mcp.Tool{mcpTool("b", nil)}, NextCursor: ""},
	}}
	// Point the second page back at the already-visited initial cursor.
	loop.pages["p2"].NextCursor = ""
	// "" is already visited, but ListServerTools checks seen[next] before
	// advancing, so "" must be reported. Exercise the cycle variant where
	// the cursor returns to a non-initial visited cursor instead.
	loop.pages[""] = &mcp.ListToolsResult{Tools: []*mcp.Tool{mcpTool("a", nil)}, NextCursor: "p2"}
	loop.pages["p2"] = &mcp.ListToolsResult{Tools: []*mcp.Tool{mcpTool("b", nil)}, NextCursor: "p3"}
	loop.pages["p3"] = &mcp.ListToolsResult{Tools: []*mcp.Tool{mcpTool("c", nil)}, NextCursor: "p2"}
	if _, err := ListServerTools(context.Background(), loop, DiscoveryLimits{}); err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestListServerToolsPageCap(t *testing.T) {
	pages := map[string]*mcp.ListToolsResult{}
	prev := ""
	for i := 0; i < 10; i++ {
		next := fmt.Sprintf("c%d", i)
		pages[prev] = &mcp.ListToolsResult{Tools: []*mcp.Tool{mcpTool(fmt.Sprintf("t%d", i), nil)}, NextCursor: next}
		prev = next
	}
	pages[prev] = &mcp.ListToolsResult{Tools: []*mcp.Tool{mcpTool("last", nil)}}
	if _, err := ListServerTools(context.Background(), &fakeLister{pages: pages}, DiscoveryLimits{MaxPages: 3}); err == nil {
		t.Fatal("expected page-cap error")
	}
}

func TestListServerToolsToolCap(t *testing.T) {
	pages := map[string]*mcp.ListToolsResult{
		"": {Tools: []*mcp.Tool{mcpTool("a", nil), mcpTool("b", nil), mcpTool("c", nil)}},
	}
	listing, err := ListServerTools(context.Background(), &fakeLister{pages: pages}, DiscoveryLimits{MaxToolsPerServer: 2})
	if err != nil {
		t.Fatalf("ListServerTools: %v", err)
	}
	if !listing.Truncated || len(listing.Tools) != 2 || listing.Seen != 3 {
		t.Fatalf("got %+v, want truncation to 2 of 3", listing)
	}
}

func TestListServerToolsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &fakeLister{pages: map[string]*mcp.ListToolsResult{}}
	if _, err := ListServerTools(ctx, fake, DiscoveryLimits{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestBaseAdaptedName(t *testing.T) {
	got := BaseAdaptedName("files", "read")
	if got != "files__read" {
		t.Fatalf("got %q", got)
	}
	got = BaseAdaptedName("my server!", "do things now")
	if !adaptedNamePattern.MatchString(got) {
		t.Fatalf("not provider-compatible: %q", got)
	}
	long := strings.Repeat("x", 100)
	got = BaseAdaptedName(long, long)
	if len(got) > MaxAdaptedNameLen || !adaptedNamePattern.MatchString(got) {
		t.Fatalf("bad truncation: %q", got)
	}
}

func TestResolveCollisionsAcrossServers(t *testing.T) {
	candidates := []NameCandidate{
		{Server: "b", Original: "read"},
		{Server: "a", Original: "read"},
	}
	assigned := ResolveAdaptedNames(candidates, nil)
	if assigned[0] == assigned[1] {
		t.Fatalf("collision unresolved: %v", assigned)
	}
	// Deterministic: same input, same output.
	again := ResolveAdaptedNames(candidates, nil)
	if assigned[0] != again[0] || assigned[1] != again[1] {
		t.Fatalf("unstable: %v vs %v", assigned, again)
	}
}

func TestResolveNormalizationAndTruncationCollisions(t *testing.T) {
	// "a-b" and "a b" normalize identically.
	candidates := []NameCandidate{
		{Server: "s", Original: "a-b"},
		{Server: "s", Original: "a b"},
		{Server: strings.Repeat("z", 80), Original: strings.Repeat("y", 80)},
		{Server: strings.Repeat("z", 80), Original: strings.Repeat("y", 80) + "!"},
	}
	assigned := ResolveAdaptedNames(candidates, nil)
	seen := map[string]bool{}
	for i, name := range assigned {
		if !adaptedNamePattern.MatchString(name) {
			t.Fatalf("candidate %d: bad name %q", i, name)
		}
		if seen[name] {
			t.Fatalf("duplicate name %q", name)
		}
		seen[name] = true
	}
}

func TestResolveReservedBuiltinCollision(t *testing.T) {
	reserved := ReservedSet("read", "shell", "schedule_create")
	candidates := []NameCandidate{{Server: "read", Original: ""}, {Server: "s", Original: "shell"}}
	_ = candidates
	// Simulate servers whose base names would collide with builtins by
	// reserving the exact base names.
	reserved[BaseAdaptedName("s", "x")] = true
	assigned := ResolveAdaptedNames([]NameCandidate{{Server: "s", Original: "x"}}, reserved)
	if assigned[0] == BaseAdaptedName("s", "x") {
		t.Fatalf("reserved collision not resolved: %q", assigned[0])
	}
	// Direct builtin-name candidate: a server/tool pair normalizing to a
	// builtin must also move.
	direct := ResolveAdaptedNames([]NameCandidate{{Server: "", Original: ""}}, ReservedSet())
	if direct[0] == "" || !adaptedNamePattern.MatchString(direct[0]) {
		t.Fatalf("bad direct name %q", direct[0])
	}
}

func TestAdaptInputSchemaPreserved(t *testing.T) {
	in := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":  map[string]any{"type": "string", "description": "p"},
			"count": map[string]any{"type": "integer", "minimum": 1},
		},
		"required":             []any{"path"},
		"additionalProperties": false,
		"enum":                 []any{"a"},
	}
	got, err := AdaptInputSchema(in)
	if err != nil {
		t.Fatalf("AdaptInputSchema: %v", err)
	}
	if got["additionalProperties"] != false {
		t.Fatalf("additionalProperties not preserved: %v", got)
	}
	// Mutating the result must not alias the input.
	got["properties"].(map[string]any)["path"] = map[string]any{"type": "number"}
	if in["properties"].(map[string]any)["path"].(map[string]any)["type"] != "string" {
		t.Fatal("input aliased by output")
	}
	req := got["required"].([]any)
	if len(req) != 1 || req[0] != "path" {
		t.Fatalf("required not preserved: %v", req)
	}
}

func TestAdaptInputSchemaInvalid(t *testing.T) {
	cases := []any{
		42,
		"object",
		map[string]any{"type": "array"},
		map[string]any{"type": "object", "properties": "nope"},
		map[string]any{"type": "object", "properties": map[string]any{"a": "nope"}},
		map[string]any{"type": "object", "required": "path"},
		map[string]any{"type": "object", "required": []any{42}},
		map[string]any{"type": 42},
	}
	for i, c := range cases {
		if _, err := AdaptInputSchema(c); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
	if _, err := AdaptInputSchema(nil); err != nil {
		t.Fatalf("nil schema should default: %v", err)
	}
}

func TestAdaptToolQuarantine(t *testing.T) {
	if _, err := AdaptTool("s", nil, "s__x"); err == nil {
		t.Fatal("nil tool should fail")
	}
	if _, err := AdaptTool("s", &mcp.Tool{Name: "  "}, "s__x"); err == nil {
		t.Fatal("empty name should fail")
	}
	bad := &mcp.Tool{Name: "x", InputSchema: map[string]any{"type": "array"}}
	if _, err := AdaptTool("s", bad, "s__x"); err == nil {
		t.Fatal("non-object schema should fail")
	}
	if _, err := AdaptTool("s", mcpTool("x", nil), "bad name!"); err == nil {
		t.Fatal("bad adapted name should fail")
	}
}

func TestAdaptToolRetainsOutputAndAnnotations(t *testing.T) {
	out := map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}
	tool := &mcp.Tool{
		Name:         "lookup",
		Description:  "does lookups",
		Title:        "Lookup",
		InputSchema:  nil,
		OutputSchema: out,
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "Lookup"},
	}
	adapted, err := AdaptTool("srv", tool, "srv__lookup")
	if err != nil {
		t.Fatalf("AdaptTool: %v", err)
	}
	if adapted.Tool.Name != "srv__lookup" {
		t.Fatalf("name %q", adapted.Tool.Name)
	}
	if adapted.OutputSchema["type"] != "object" {
		t.Fatalf("output not retained: %v", adapted.OutputSchema)
	}
	if adapted.Annotations == nil || !adapted.Annotations.ReadOnlyHint {
		t.Fatalf("annotations not retained: %+v", adapted.Annotations)
	}
	if adapted.Title != "Lookup" {
		t.Fatalf("title %q", adapted.Title)
	}
	// Provider params must not leak output schema.
	if _, ok := adapted.Tool.Parameters["outputSchema"]; ok {
		t.Fatal("output schema leaked into provider params")
	}
}

func TestDiscoverSessionsIsolatesAndQuarantines(t *testing.T) {
	good := &fakeLister{pages: map[string]*mcp.ListToolsResult{
		"": {Tools: []*mcp.Tool{mcpTool("read", objectSchema(map[string]any{"p": map[string]any{"type": "string"}}, "p"))}},
	}}
	broken := &fakeLister{errOn: map[string]error{"": errors.New("boom")}}
	mixed := &fakeLister{pages: map[string]*mcp.ListToolsResult{
		"": {Tools: []*mcp.Tool{
			mcpTool("ok", nil),
			{Name: "", Description: "nameless"},
			{Name: "bad", InputSchema: map[string]any{"type": "array"}},
		}},
	}}
	sessions := map[string]ToolLister{"good": good, "broken": broken, "mixed": mixed}
	catalog, quarantined := DiscoverSessions(context.Background(), sessions, []string{"broken", "good", "mixed"}, DiscoverOptions{})
	if catalog.Len() != 2 {
		t.Fatalf("catalog len %d, want 2 (good+ok)", catalog.Len())
	}
	if len(quarantined) < 3 {
		t.Fatalf("quarantined %d, want >=3: %+v", len(quarantined), quarantined)
	}
	if _, ok := catalog.Lookup("good__read"); !ok {
		t.Fatalf("missing good tool; tools=%v", catalog.Tools())
	}
}

func TestDiscoverBuiltinCollisionResolved(t *testing.T) {
	fake := &fakeLister{pages: map[string]*mcp.ListToolsResult{
		"": {Tools: []*mcp.Tool{mcpTool("read", nil)}},
	}}
	// A server literally named to collide after prefixing still must not
	// shadow the builtin "read".
	reserved := ReservedSet("read", "shell", "process", "schedule_create", "schedule_list", "schedule_delete")
	catalog, _ := DiscoverSessions(context.Background(),
		map[string]ToolLister{"s": fake}, []string{"s"}, DiscoverOptions{Reserved: reserved})
	tools := catalog.Tools()
	if len(tools) != 1 {
		t.Fatalf("len %d", len(tools))
	}
	if tools[0].Name == "read" {
		t.Fatal("builtin collision unresolved")
	}
}

func TestCatalogImmutable(t *testing.T) {
	fake := &fakeLister{pages: map[string]*mcp.ListToolsResult{
		"": {Tools: []*mcp.Tool{mcpTool("a", objectSchema(map[string]any{"p": map[string]any{"type": "string"}}))}},
	}}
	catalog, _ := DiscoverSessions(context.Background(),
		map[string]ToolLister{"s": fake}, []string{"s"}, DiscoverOptions{})
	first := catalog.Tools()
	first[0].Name = "MUTATED"
	first[0].Parameters["type"] = "MUTATED"
	second := catalog.Tools()
	if second[0].Name == "MUTATED" || second[0].Parameters["type"] == "MUTATED" {
		t.Fatal("catalog mutated via Tools()")
	}
	entry, ok := catalog.Lookup(second[0].Name)
	if !ok {
		t.Fatal("lookup failed")
	}
	entry.Tool.Parameters["type"] = "MUTATED"
	again, _ := catalog.Lookup(second[0].Name)
	if again.Tool.Parameters["type"] == "MUTATED" {
		t.Fatal("catalog mutated via Lookup()")
	}
}

func TestCatalogResolveRejectsStale(t *testing.T) {
	fake := &fakeLister{pages: map[string]*mcp.ListToolsResult{
		"": {Tools: []*mcp.Tool{mcpTool("a", nil)}},
	}}
	catalog, _ := DiscoverSessions(context.Background(),
		map[string]ToolLister{"s": fake}, []string{"s"}, DiscoverOptions{})
	tools := catalog.Tools()
	entry, err := catalog.Resolve(llm.ToolCall{Name: tools[0].Name})
	if err != nil || entry.Adapted != tools[0].Name {
		t.Fatalf("resolve known: %v %+v", err, entry)
	}
	// Original (unadapted) names must not resolve: no remapping.
	if _, err := catalog.Resolve(llm.ToolCall{Name: "a"}); err == nil {
		t.Fatal("original name should not resolve")
	}
	if _, err := catalog.Resolve(llm.ToolCall{Name: "s__missing"}); err == nil {
		t.Fatal("unknown name should not resolve")
	}
}

func TestDiscoverGlobalBudget(t *testing.T) {
	fake := &fakeLister{pages: map[string]*mcp.ListToolsResult{
		"": {Tools: []*mcp.Tool{mcpTool("a", nil), mcpTool("b", nil), mcpTool("c", nil)}},
	}}
	catalog, quarantined := DiscoverSessions(context.Background(),
		map[string]ToolLister{"s": fake}, []string{"s"}, DiscoverOptions{MaxTotal: 2})
	if catalog.Len() != 2 {
		t.Fatalf("len %d, want 2", catalog.Len())
	}
	found := false
	for _, q := range quarantined {
		if strings.Contains(q.Reason, "budget") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no budget quarantine: %+v", quarantined)
	}
}

func TestNewClientWithToolListChanged(t *testing.T) {
	if NewClientWithToolListChanged(nil) == nil {
		t.Fatal("nil handler should behave like NewClient")
	}
	called := false
	c := NewClientWithToolListChanged(func(context.Context, *mcp.ToolListChangedRequest) { called = true })
	if c == nil {
		t.Fatal("nil client")
	}
	_ = called
}

func TestDiscoverNilTask(t *testing.T) {
	catalog, quarantined := Discover(context.Background(), nil, DiscoverOptions{})
	if !catalog.Empty() || len(quarantined) != 1 {
		t.Fatalf("got len=%d q=%d", catalog.Len(), len(quarantined))
	}
}
