package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
	"github.com/looprig/storage/memstore"
	"github.com/looprig/tools/bash"
)

// retentionPreviewBytes is the loop's finite model preview budget. With an
// unbounded preview nothing is ever elided and nothing is retained.
const retentionPreviewBytes = 4096

var (
	markerPattern = regexp.MustCompile(`read_tool_result capture_id="([0-9a-f-]{36})"`)
	footerPattern = regexp.MustCompile(`\n\[capture ([0-9a-f-]{36}): bytes (\d+)-(\d+) of (\d+) retained[^\]]*; (next_offset=(\d+)|end)[^\]]*\]$`)
)

// memToolResultObjects is an in-memory loop.ToolResultObjects: it verifies
// what it is given and mints its own references, the way SessionStore does.
type memToolResultObjects struct {
	mu        sync.Mutex
	objects   map[string][]byte
	publishes int
}

func objectKey(session uuid.UUID, objectID string) string { return session.String() + "/" + objectID }

func (m *memToolResultObjects) PublishToolResultObject(_ context.Context, session uuid.UUID, content io.Reader, size uint64, sum [32]byte) (sessionwire.ObjectMetadata, error) {
	data, err := io.ReadAll(content)
	if err != nil {
		return sessionwire.ObjectMetadata{}, err
	}
	if uint64(len(data)) != size || sha256.Sum256(data) != sum {
		return sessionwire.ObjectMetadata{}, errors.New("published bytes do not match their declared size and digest")
	}
	digest := hex.EncodeToString(sum[:])
	reference := sessionwire.ObjectReference{ObjectID: "v1:tool-result:1:" + digest}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.objects == nil {
		m.objects = make(map[string][]byte)
	}
	m.objects[objectKey(session, reference.ObjectID)] = data
	m.publishes++
	return sessionwire.ObjectMetadata{Reference: reference, SizeBytes: size, Digest: "sha256:" + digest}, nil
}

func (m *memToolResultObjects) OpenToolResultObject(_ context.Context, session uuid.UUID, reference sessionwire.ObjectReference) (sessionwire.ObjectMetadata, io.ReadCloser, error) {
	m.mu.Lock()
	data, ok := m.objects[objectKey(session, reference.ObjectID)]
	m.mu.Unlock()
	if !ok {
		return sessionwire.ObjectMetadata{}, nil, errors.New("no such object")
	}
	sum := sha256.Sum256(data)
	return sessionwire.ObjectMetadata{Reference: reference, SizeBytes: uint64(len(data)), Digest: "sha256:" + hex.EncodeToString(sum[:])},
		io.NopCloser(bytes.NewReader(data)), nil
}

func (m *memToolResultObjects) snapshot() (int, [][]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all [][]byte
	for _, data := range m.objects {
		all = append(all, data)
	}
	return m.publishes, all
}

// pagingLLM behaves like a model that follows the retention marker:
//
//   - "run <command>" → call Bash with that command;
//   - a result naming read_tool_result → read the capture from offset 0;
//   - a page whose footer reports next_offset → read the next page;
//   - "read <id>" → read that capture id;
//   - anything else → answer "done".
//
// Every page it is shown is kept, so the test can reassemble them.
type pagingLLM struct {
	mu    sync.Mutex
	calls int
	pages []string
}

func (*pagingLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("paging llm: Invoke is unused")
}

func (l *pagingLLM) Stream(_ context.Context, request inference.Request) (*stream.StreamReader[content.Chunk], error) {
	l.mu.Lock()
	l.calls++
	id := "use-" + strconv.Itoa(l.calls)
	last := request.Messages[len(request.Messages)-1]
	text := conversationText(last)
	_, fromUser := last.(*content.UserMessage)
	var chunks []content.Chunk
	readCall := func(captureID string, offset string) []content.Chunk {
		return []content.Chunk{&content.ToolUseChunk{Index: 0, ID: id, Name: loop.ReadToolResultToolName,
			InputJSON: `{"capture_id":"` + captureID + `","offset":` + offset + `}`}}
	}
	switch {
	case fromUser && strings.HasPrefix(text, "run "):
		args, _ := json.Marshal(map[string]any{"command": strings.TrimPrefix(text, "run ")})
		chunks = []content.Chunk{&content.ToolUseChunk{Index: 0, ID: id, Name: "Bash", InputJSON: string(args)}}
	case fromUser && strings.HasPrefix(text, "read "):
		chunks = readCall(strings.TrimPrefix(text, "read "), "0")
	case !fromUser && footerPattern.MatchString(text):
		l.pages = append(l.pages, text)
		if match := footerPattern.FindStringSubmatch(text); match[6] != "" {
			chunks = readCall(match[1], match[6])
		}
	case !fromUser && markerPattern.MatchString(text):
		chunks = readCall(markerPattern.FindStringSubmatch(text)[1], "0")
	case !fromUser && strings.HasPrefix(text, "error: read tool result"):
		l.pages = append(l.pages, text)
	}
	l.mu.Unlock()
	if chunks == nil {
		chunks = []content.Chunk{&content.TextChunk{Text: "done"}}
	}
	index := 0
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if index == len(chunks) {
			return nil, io.EOF
		}
		chunk := chunks[index]
		index++
		return chunk, nil
	}, nil), nil
}

func (l *pagingLLM) takePages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	pages := l.pages
	l.pages = nil
	return pages
}

func conversationText(message content.Conversation) string {
	var blocks []content.Block
	switch m := message.(type) {
	case *content.UserMessage:
		blocks = m.Blocks
	case *content.ToolResultMessage:
		blocks = m.Blocks
	case *content.AIMessage:
		blocks = m.Blocks
	}
	var b strings.Builder
	for _, block := range blocks {
		if text, ok := block.(*content.TextBlock); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

type allowAll struct{}

func (allowAll) AccessVersion() uint16                   { return gate.CurrentAccessVersion }
func (allowAll) AccessFor(string, string) (uint8, error) { return gate.AccessAllow, nil }

type discardRules struct{}

func (discardRules) WriteRules(context.Context, []tool.RuleCandidate) error { return nil }

func retentionSession(t *testing.T, ctx context.Context, objects *memToolResultObjects, llm *pagingLLM) session.SessionController {
	t.Helper()
	evaluator, err := gate.NewInteractiveEvaluator(
		[]gate.AccessBinding{{Kind: tool.CapabilityCommandExecute, Source: allowAll{}}},
		nil, loop.GateApprover(), discardRules{}, nil,
	)
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator: %v", err)
	}
	root := t.TempDir()
	bashDefinition := tool.NewDefinition("Bash", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{bash.NewBash(root)}, nil
	})
	definition, err := loop.Define(
		loop.WithName("agent"),
		loop.WithInference(llm, model.Model{Provider: "test", APIFormat: model.APIFormatOpenAI, BaseURL: "http://localhost", Name: "retention"}),
		loop.WithTools(bashDefinition, ReadToolResultDefinition()),
		loop.WithToolLimits(loop.ToolLimits{ResultBytes: retentionPreviewBytes}),
		loop.WithAccessGate(evaluator),
		loop.WithPolicyRevision("retention-v1"),
	)
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	// The journal lives in an in-memory harness store; the retained objects
	// live in objects, so the test sees exactly what was published.
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("sessionstore.Open: %v", err)
	}
	r, err := rig.Define(rig.WithLoops(definition), rig.WithPrimers("agent"), rig.WithSessionStore(store), rig.WithToolResultObjects(objects, t.TempDir()))
	if err != nil {
		t.Fatalf("rig.Define: %v", err)
	}
	live, err := r.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = live.Shutdown(context.Background()) })
	return live
}

// runTurn submits text and waits for the turn to end, returning every
// StepDone capture committed during it.
func runTurn(t *testing.T, ctx context.Context, controller session.SessionController, text string) []event.ToolResultCapture {
	t.Helper()
	sub, err := controller.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer sub.Close()
	if _, err := controller.Submit(ctx, []content.Block{&content.TextBlock{Text: text}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	var captures []event.ToolResultCapture
	for {
		select {
		case delivery := <-sub.Events():
			if done, ok := delivery.Event.(event.StepDone); ok {
				captures = append(captures, done.Captures...)
			}
			if delivery.Event.EndsTurn() {
				if failed, ok := delivery.Event.(event.TurnFailed); ok {
					t.Fatalf("turn %q failed: %v", text, failed.Err)
				}
				return captures
			}
		case <-ctx.Done():
			t.Fatalf("turn %q timed out", text)
		}
	}
}

// TestBashOutputIsRetainedAndPagedBackThroughReadToolResult is T0 and T1 end
// to end through Harness's real capture pipeline:
//
//   - a small Bash result is its own complete capture, so no object is stored;
//   - a large Bash result streams every byte to the sink, the store receives
//     the complete output (not Bash's 32 KiB preview), and the model's marker
//     names read_tool_result;
//   - the real read_tool_result tool pages the retained bytes back, every page
//     fits the preview budget (so no page is itself retained), and the pages
//     reassemble the complete output exactly;
//   - a capture id the loop did not produce fails closed.
func TestBashOutputIsRetainedAndPagedBackThroughReadToolResult(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh not available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	objects := &memToolResultObjects{}
	llm := &pagingLLM{}
	live := retentionSession(t, ctx, objects, llm)

	small := runTurn(t, ctx, live, "run printf 'hello\\n'")
	if publishes, _ := objects.snapshot(); publishes != 0 || len(small) != 1 || small[0].Reference != nil {
		t.Fatalf("small result: %d publishes, captures %+v; want one reference-free capture", publishes, small)
	}

	const lines = 3000
	var want strings.Builder
	for i := range lines {
		fmt.Fprintf(&want, "build log line %05d\n", i)
	}
	want.WriteString("[exit code: 0]")
	command := `i=0; while [ "$i" -lt ` + strconv.Itoa(lines) + ` ]; do printf 'build log line %05d\n' "$i"; i=$((i + 1)); done`
	captures := runTurn(t, ctx, live, "run "+command)

	publishes, stored := objects.snapshot()
	if publishes != 1 || len(stored) != 1 {
		t.Fatalf("large result: %d publishes, want exactly 1 (pages must not be retained)", publishes)
	}
	if string(stored[0]) != want.String() {
		t.Fatalf("stored object is %d bytes, want the complete %d-byte output (not Bash's preview)", len(stored[0]), want.Len())
	}
	var bashCapture *event.ToolResultCapture
	for i := range captures {
		if captures[i].Reference != nil {
			if bashCapture != nil {
				t.Fatalf("more than one capture carries a reference: %+v", captures)
			}
			bashCapture = &captures[i]
		}
	}
	if bashCapture == nil || bashCapture.CapturedBytes != uint64(want.Len()) || bashCapture.Truncated {
		t.Fatalf("Bash capture = %+v, want an untruncated %d-byte reference capture", bashCapture, want.Len())
	}

	pages := llm.takePages()
	if len(pages) < 2 {
		t.Fatalf("the model read %d pages, want the whole capture over several", len(pages))
	}
	var reassembled strings.Builder
	for i, page := range pages {
		if len(page) > retentionPreviewBytes {
			t.Fatalf("page %d is %d bytes, over the %d-byte preview budget", i, len(page), retentionPreviewBytes)
		}
		match := footerPattern.FindStringSubmatchIndex(page)
		if match == nil {
			t.Fatalf("page %d has no footer: %q", i, page)
		}
		if got := page[match[2]:match[3]]; got != bashCapture.ToolExecutionID.String() {
			t.Fatalf("page %d names capture %s, want %s", i, got, bashCapture.ToolExecutionID)
		}
		start, _ := strconv.Atoi(page[match[4]:match[5]])
		if start != reassembled.Len() {
			t.Fatalf("page %d starts at %d, want %d", i, start, reassembled.Len())
		}
		reassembled.WriteString(page[:match[0]])
	}
	if reassembled.String() != want.String() {
		t.Fatalf("reassembled %d bytes, want the complete %d-byte output", reassembled.Len(), want.Len())
	}

	runTurn(t, ctx, live, "read 00000000-0000-4000-8000-000000000000")
	if refused := llm.takePages(); len(refused) != 1 || refused[0] != "error: read tool result: unknown capture_id" {
		t.Fatalf("foreign capture read = %q, want the unknown-capture refusal", refused)
	}
}
