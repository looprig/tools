package task

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
)

var errSequenceExhausted = errors.New("test id sequence exhausted")

func sequenceIDs(ids ...uuid.UUID) idSource {
	index := 0
	return func() (uuid.UUID, error) {
		if index >= len(ids) {
			return uuid.UUID{}, errSequenceExhausted
		}
		id := ids[index]
		index++
		return id, nil
	}
}

func testUUID(value byte) uuid.UUID {
	return uuid.UUID{value, 0, 0, 0, 0, 0, 0x40, 0, 0x80, 0, 0, 0, 0, 0, 0, value}
}

func validCreateInput(subject string) createInput {
	return createInput{
		Subject:     subject,
		Description: "description",
	}
}

func mustCreate(t *testing.T, store *store, input createInput) Task {
	t.Helper()
	task, err := store.create(input)
	if err != nil {
		t.Fatalf("store.create() error = %v", err)
	}
	return task
}

func snapshotRecords(t *testing.T, store *store) map[string]taskRecord {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()

	snapshot := make(map[string]taskRecord, len(store.tasks))
	for id, record := range store.tasks {
		snapshot[id] = cloneRecord(record)
	}
	return snapshot
}

func assertRecordsUnchanged(t *testing.T, store *store, before map[string]taskRecord) {
	t.Helper()
	after := snapshotRecords(t, store)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected create mutated records:\n before: %#v\n after:  %#v", before, after)
	}
}

func idsFor(count int) []uuid.UUID {
	ids := make([]uuid.UUID, count)
	for i := range ids {
		ids[i] = testUUID(byte(i + 1))
	}
	return ids
}

func TestStoreCreateDefaultsStatusAndGeneratesCanonicalID(t *testing.T) {
	id := testUUID(1)
	store := newStore(sequenceIDs(id))

	got, err := store.create(validCreateInput("subject"))
	if err != nil {
		t.Fatalf("store.create() error = %v", err)
	}
	if got.Status != StatusPending {
		t.Fatalf("created status = %q, want %q", got.Status, StatusPending)
	}
	if got.ID == "" {
		t.Fatal("created ID is empty")
	}
	parsed, err := uuid.Parse(got.ID)
	if err != nil {
		t.Fatalf("created ID %q is not a UUID: %v", got.ID, err)
	}
	if got.ID != parsed.String() || got.ID != id.String() {
		t.Fatalf("created ID = %q, want canonical %q", got.ID, id.String())
	}
}

func TestStoreIDSourceFailureLeavesGraphUnchanged(t *testing.T) {
	wantErr := errors.New("source failed")
	store := newStore(func() (uuid.UUID, error) {
		return uuid.UUID{}, wantErr
	})
	before := snapshotRecords(t, store)

	if _, err := store.create(validCreateInput("subject")); !errors.Is(err, wantErr) {
		t.Fatalf("store.create() error = %v, want %v", err, wantErr)
	}
	assertRecordsUnchanged(t, store, before)
}

func TestStoreIDCollisionLeavesGraphUnchanged(t *testing.T) {
	id := testUUID(1)
	store := newStore(sequenceIDs(id, id))
	mustCreate(t, store, validCreateInput("first"))
	before := snapshotRecords(t, store)

	if _, err := store.create(validCreateInput("collision")); err == nil {
		t.Fatal("store.create() succeeded for a generated ID collision")
	}
	assertRecordsUnchanged(t, store, before)
}

func TestStoreCreateRejectsEmptyAndWhitespaceOnlyFields(t *testing.T) {
	tests := []struct {
		name  string
		input createInput
	}{
		{name: "empty subject", input: createInput{Subject: "", Description: "description"}},
		{name: "whitespace subject", input: createInput{Subject: " \t\n", Description: "description"}},
		{name: "empty description", input: createInput{Subject: "subject", Description: ""}},
		{name: "whitespace description", input: createInput{Subject: "subject", Description: " \t\n"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(sequenceIDs(testUUID(1)))
			before := snapshotRecords(t, store)

			if _, err := store.create(tt.input); err == nil {
				t.Fatal("store.create() accepted an empty or whitespace-only field")
			}
			assertRecordsUnchanged(t, store, before)
		})
	}
}

func TestStoreCreateRejectsUnknownDependencyAtomically(t *testing.T) {
	store := newStore(sequenceIDs(testUUID(1)))
	before := snapshotRecords(t, store)
	input := validCreateInput("subject")
	input.BlockedBy = []string{testUUID(99).String()}

	if _, err := store.create(input); err == nil {
		t.Fatal("store.create() accepted an unknown dependency")
	}
	assertRecordsUnchanged(t, store, before)
}

func TestStoreCreateNormalizesDependenciesDeterministically(t *testing.T) {
	dependencyID := testUUID(1)
	taskID := testUUID(2)
	store := newStore(sequenceIDs(dependencyID, taskID))
	mustCreate(t, store, validCreateInput("dependency"))

	input := validCreateInput("dependent")
	input.BlockedBy = []string{
		strings.ToUpper(dependencyID.String()),
		dependencyID.String(),
		dependencyID.String(),
	}
	got := mustCreate(t, store, input)

	if want := []string{dependencyID.String()}; !reflect.DeepEqual(got.BlockedBy, want) {
		t.Fatalf("normalized BlockedBy = %#v, want %#v", got.BlockedBy, want)
	}
}

func TestStoreGetRejectsUnknownAndInvalidID(t *testing.T) {
	store := newStore(sequenceIDs(testUUID(1)))
	for _, id := range []string{testUUID(99).String(), "not-a-uuid"} {
		if _, err := store.get(id); err == nil {
			t.Errorf("store.get(%q) succeeded, want error", id)
		}
	}
}

func TestStoreGetSnapshotIsDefensive(t *testing.T) {
	dependencyID := testUUID(1)
	taskID := testUUID(2)
	store := newStore(sequenceIDs(dependencyID, taskID))
	mustCreate(t, store, validCreateInput("dependency"))
	input := validCreateInput("dependent")
	input.BlockedBy = []string{dependencyID.String()}
	input.Metadata = json.RawMessage(`{"component":"parser"}`)
	mustCreate(t, store, input)

	got, err := store.get(taskID.String())
	if err != nil {
		t.Fatalf("store.get() error = %v", err)
	}
	got.BlockedBy[0] = testUUID(3).String()
	got.Metadata[0] = '['
	got.Blocks = append(got.Blocks, testUUID(4).String())

	again, err := store.get(taskID.String())
	if err != nil {
		t.Fatalf("second store.get() error = %v", err)
	}
	if want := []string{dependencyID.String()}; !reflect.DeepEqual(again.BlockedBy, want) {
		t.Fatalf("stored BlockedBy changed through get snapshot: %#v", again.BlockedBy)
	}
	if gotMetadata := string(again.Metadata); gotMetadata != `{"component":"parser"}` {
		t.Fatalf("stored Metadata changed through get snapshot: %q", gotMetadata)
	}
	if len(again.Blocks) != 0 {
		t.Fatalf("dependent task Blocks = %#v, want no blocks", again.Blocks)
	}
}

func TestStoreListOrdersByCanonicalUUIDAndReturnsDefensiveSnapshots(t *testing.T) {
	firstID := testUUID(1)
	secondID := testUUID(2)
	store := newStore(sequenceIDs(secondID, firstID))
	second := validCreateInput("second")
	second.Metadata = json.RawMessage(`{"name":"second"}`)
	mustCreate(t, store, second)
	first := validCreateInput("first")
	first.Metadata = json.RawMessage(`{"name":"first"}`)
	mustCreate(t, store, first)

	listed := store.list()
	if got, want := []string{listed[0].ID, listed[1].ID}, []string{firstID.String(), secondID.String()}; !reflect.DeepEqual(got, want) {
		t.Fatalf("list IDs = %#v, want %#v", got, want)
	}
	listed[0].Subject = "changed"
	listed[0].Metadata[0] = '['
	listed[1].Metadata[0] = '['

	again := store.list()
	if again[0].Subject != "first" || again[1].Subject != "second" {
		t.Fatalf("list snapshot mutation changed stored subjects: %#v", again)
	}
	if string(again[0].Metadata) != `{"name":"first"}` || string(again[1].Metadata) != `{"name":"second"}` {
		t.Fatalf("list snapshot mutation changed stored metadata: %#v", again)
	}
}

func TestStoreDerivedBlocksFromOtherRecords(t *testing.T) {
	dependencyID := testUUID(1)
	dependentBID := testUUID(2)
	dependentAID := testUUID(3)
	store := newStore(sequenceIDs(dependencyID, dependentBID, dependentAID))
	mustCreate(t, store, validCreateInput("dependency"))
	dependentB := validCreateInput("dependent b")
	dependentB.BlockedBy = []string{dependencyID.String()}
	mustCreate(t, store, dependentB)
	dependentA := validCreateInput("dependent a")
	dependentA.BlockedBy = []string{dependencyID.String()}
	mustCreate(t, store, dependentA)

	got, err := store.get(dependencyID.String())
	if err != nil {
		t.Fatalf("store.get() error = %v", err)
	}
	want := []string{dependentBID.String(), dependentAID.String()}
	if !reflect.DeepEqual(got.Blocks, want) {
		t.Fatalf("derived Blocks = %#v, want %#v", got.Blocks, want)
	}
}

func TestStoreLimitsCountAndFieldBytesRejectAtomically(t *testing.T) {
	t.Run("count", func(t *testing.T) {
		ids := idsFor(maxTasksPerLoop + 1)
		store := newStore(sequenceIDs(ids...))
		for i := 0; i < maxTasksPerLoop; i++ {
			mustCreate(t, store, validCreateInput("subject"))
		}
		before := snapshotRecords(t, store)
		if _, err := store.create(validCreateInput("too many")); err == nil {
			t.Fatal("store.create() exceeded maxTasksPerLoop")
		}
		assertRecordsUnchanged(t, store, before)
	})

	tests := []struct {
		name  string
		input createInput
	}{
		{name: "subject", input: createInput{Subject: strings.Repeat("s", maxSubjectBytes+1), Description: "description"}},
		{name: "description", input: createInput{Subject: "subject", Description: strings.Repeat("d", maxDescriptionBytes+1)}},
		{name: "active form", input: createInput{Subject: "subject", Description: "description", ActiveForm: strings.Repeat("a", maxActiveFormBytes+1)}},
		{name: "metadata", input: createInput{Subject: "subject", Description: "description", Metadata: json.RawMessage(`{"value":"` + strings.Repeat("m", maxMetadataBytes) + `"}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(sequenceIDs(testUUID(1)))
			before := snapshotRecords(t, store)
			if _, err := store.create(tt.input); err == nil {
				t.Fatal("store.create() accepted an oversized field")
			}
			assertRecordsUnchanged(t, store, before)
		})
	}
}

func TestStoreLimitsRequestArgumentBytesRejectAtomically(t *testing.T) {
	store := newStore(sequenceIDs(testUUID(1)))
	before := snapshotRecords(t, store)

	if _, err := store.create(validCreateInput("subject"), maxTaskArgsBytes+1); err == nil {
		t.Fatal("store.create() accepted an oversized request argument")
	}
	assertRecordsUnchanged(t, store, before)
}

func TestStoreLimitsDependencyCountRejectsAtomically(t *testing.T) {
	ids := idsFor(maxDependencies + 2)
	store := newStore(sequenceIDs(ids...))
	for i := 0; i < maxDependencies+1; i++ {
		mustCreate(t, store, validCreateInput("dependency"))
	}

	input := validCreateInput("dependent")
	input.BlockedBy = make([]string, maxDependencies+1)
	for i := range input.BlockedBy {
		input.BlockedBy[i] = ids[i].String()
	}
	before := snapshotRecords(t, store)
	if _, err := store.create(input); err == nil {
		t.Fatal("store.create() accepted too many dependencies")
	}
	assertRecordsUnchanged(t, store, before)
}

func TestStoreLimitsAggregateBytesRejectAtomically(t *testing.T) {
	ids := idsFor(maxTasksPerLoop)
	store := newStore(sequenceIDs(ids...))
	for i := 0; i < 127; i++ {
		input := createInput{
			Subject:     "s",
			Description: strings.Repeat("d", maxDescriptionBytes),
		}
		mustCreate(t, store, input)
	}

	before := snapshotRecords(t, store)
	input := createInput{
		Subject:     "s",
		Description: strings.Repeat("d", maxDescriptionBytes),
	}
	if _, err := store.create(input); err == nil {
		t.Fatal("store.create() exceeded maxStoreBytes")
	}
	assertRecordsUnchanged(t, store, before)
}
