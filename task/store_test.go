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

var _ func(*store, createInput) (Task, error) = (*store).create

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

func TestStoreZeroIDLeavesGraphUnchanged(t *testing.T) {
	store := newStore(sequenceIDs(uuid.UUID{}))
	before := snapshotRecords(t, store)

	if _, err := store.create(validCreateInput("zero ID")); err == nil {
		t.Fatal("store.create() accepted a zero UUID")
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

func stringPointer(value string) *string {
	return &value
}

func statusPointer(value Status) *Status {
	return &value
}

func metadataPointer(value string) *json.RawMessage {
	metadata := json.RawMessage(value)
	return &metadata
}

func mustUpdate(t *testing.T, store *store, input updateInput) Task {
	t.Helper()
	updated, deleted, err := store.update(input)
	if err != nil {
		t.Fatalf("store.update() error = %v", err)
	}
	if deleted {
		t.Fatal("store.update() unexpectedly deleted task")
	}
	return updated
}

func TestStoreUpdateOmittedFieldsRemainUnchanged(t *testing.T) {
	dependencyID := testUUID(1)
	taskID := testUUID(2)
	store := newStore(sequenceIDs(dependencyID, taskID))
	mustCreate(t, store, validCreateInput("dependency"))
	input := validCreateInput("original subject")
	input.ActiveForm = "working on original"
	input.BlockedBy = []string{dependencyID.String()}
	input.Metadata = json.RawMessage(`{"owner":"original"}`)
	mustCreate(t, store, input)

	updated := mustUpdate(t, store, updateInput{
		TaskID:  taskID.String(),
		Subject: stringPointer("updated subject"),
	})
	if updated.Subject != "updated subject" {
		t.Fatalf("updated Subject = %q, want %q", updated.Subject, "updated subject")
	}
	if updated.Description != "description" {
		t.Fatalf("omitted Description = %q, want %q", updated.Description, "description")
	}
	if updated.ActiveForm != "working on original" {
		t.Fatalf("omitted ActiveForm = %q, want %q", updated.ActiveForm, "working on original")
	}
	if updated.Status != StatusPending {
		t.Fatalf("omitted Status = %q, want %q", updated.Status, StatusPending)
	}
	if want := []string{dependencyID.String()}; !reflect.DeepEqual(updated.BlockedBy, want) {
		t.Fatalf("omitted BlockedBy = %#v, want %#v", updated.BlockedBy, want)
	}
	if string(updated.Metadata) != `{"owner":"original"}` {
		t.Fatalf("omitted Metadata = %q, want original metadata", updated.Metadata)
	}
}

func TestStoreMetadataUpdateReplacesExistingMetadata(t *testing.T) {
	taskID := testUUID(1)
	store := newStore(sequenceIDs(taskID))
	input := validCreateInput("subject")
	input.Metadata = json.RawMessage(`{"owner":"original"}`)
	mustCreate(t, store, input)

	updated := mustUpdate(t, store, updateInput{
		TaskID:   taskID.String(),
		Metadata: metadataPointer(`{"owner":"replacement","priority":2}`),
	})
	if got, want := string(updated.Metadata), `{"owner":"replacement","priority":2}`; got != want {
		t.Fatalf("updated Metadata = %q, want %q", got, want)
	}
}

func TestStoreMetadataEmptyObjectClearsMetadata(t *testing.T) {
	taskID := testUUID(1)
	store := newStore(sequenceIDs(taskID))
	input := validCreateInput("subject")
	input.Metadata = json.RawMessage(`{"owner":"original"}`)
	mustCreate(t, store, input)

	updated := mustUpdate(t, store, updateInput{
		TaskID:   taskID.String(),
		Metadata: metadataPointer(`{}`),
	})
	if updated.Metadata != nil {
		t.Fatalf("empty metadata object produced %q, want nil", updated.Metadata)
	}
	again, err := store.get(taskID.String())
	if err != nil {
		t.Fatalf("store.get() error = %v", err)
	}
	if again.Metadata != nil {
		t.Fatalf("stored empty metadata object = %q, want nil", again.Metadata)
	}
}

func TestStoreUpdateRejectsUnknownStatusAndTaskIDAtomically(t *testing.T) {
	taskID := testUUID(1)
	store := newStore(sequenceIDs(taskID))
	mustCreate(t, store, validCreateInput("subject"))
	unknownStatus := Status("unknown")

	tests := []struct {
		name  string
		input updateInput
	}{
		{
			name:  "unknown status",
			input: updateInput{TaskID: taskID.String(), Status: &unknownStatus},
		},
		{
			name:  "unknown task ID",
			input: updateInput{TaskID: testUUID(99).String(), Subject: stringPointer("changed")},
		},
		{
			name:  "invalid task ID",
			input: updateInput{TaskID: "not-a-uuid", Subject: stringPointer("changed")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := snapshotRecords(t, store)
			if _, _, err := store.update(tt.input); err == nil {
				t.Fatal("store.update() succeeded, want error")
			}
			assertRecordsUnchanged(t, store, before)
		})
	}
}

func TestStoreDependencyUpdateRejectsUnknownAndSelfDependencyAtomically(t *testing.T) {
	taskID := testUUID(1)
	store := newStore(sequenceIDs(taskID))
	mustCreate(t, store, validCreateInput("subject"))

	tests := []struct {
		name string
		deps []string
	}{
		{name: "unknown dependency", deps: []string{testUUID(99).String()}},
		{name: "self dependency", deps: []string{taskID.String()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := snapshotRecords(t, store)
			if _, _, err := store.update(updateInput{TaskID: taskID.String(), AddBlockedBy: tt.deps}); err == nil {
				t.Fatal("store.update() accepted an invalid dependency")
			}
			assertRecordsUnchanged(t, store, before)
		})
	}
}

func TestStoreCycleRejectsTwoNodeCycleAtomically(t *testing.T) {
	firstID := testUUID(1)
	secondID := testUUID(2)
	store := newStore(sequenceIDs(firstID, secondID))
	mustCreate(t, store, validCreateInput("first"))
	mustCreate(t, store, validCreateInput("second"))
	mustUpdate(t, store, updateInput{TaskID: firstID.String(), AddBlockedBy: []string{secondID.String()}})

	before := snapshotRecords(t, store)
	if _, _, err := store.update(updateInput{TaskID: secondID.String(), AddBlockedBy: []string{firstID.String()}}); err == nil {
		t.Fatal("store.update() accepted a two-node dependency cycle")
	}
	assertRecordsUnchanged(t, store, before)
}

func TestStoreCycleRejectsThreeNodeCycleAtomically(t *testing.T) {
	firstID := testUUID(1)
	secondID := testUUID(2)
	thirdID := testUUID(3)
	store := newStore(sequenceIDs(firstID, secondID, thirdID))
	mustCreate(t, store, validCreateInput("first"))
	mustCreate(t, store, validCreateInput("second"))
	mustCreate(t, store, validCreateInput("third"))
	mustUpdate(t, store, updateInput{TaskID: firstID.String(), AddBlockedBy: []string{secondID.String()}})
	mustUpdate(t, store, updateInput{TaskID: secondID.String(), AddBlockedBy: []string{thirdID.String()}})

	before := snapshotRecords(t, store)
	if _, _, err := store.update(updateInput{TaskID: thirdID.String(), AddBlockedBy: []string{firstID.String()}}); err == nil {
		t.Fatal("store.update() accepted a three-node dependency cycle")
	}
	assertRecordsUnchanged(t, store, before)
}

func TestStoreDependencyUpdatesNormalizeDuplicatesAndRemovalWins(t *testing.T) {
	firstID := testUUID(1)
	secondID := testUUID(2)
	taskID := testUUID(3)
	store := newStore(sequenceIDs(firstID, secondID, taskID))
	mustCreate(t, store, validCreateInput("first"))
	mustCreate(t, store, validCreateInput("second"))
	mustCreate(t, store, validCreateInput("target"))

	updated := mustUpdate(t, store, updateInput{
		TaskID:          taskID.String(),
		AddBlockedBy:    []string{secondID.String(), firstID.String(), firstID.String(), secondID.String()},
		RemoveBlockedBy: []string{firstID.String(), firstID.String()},
	})
	if want := []string{secondID.String()}; !reflect.DeepEqual(updated.BlockedBy, want) {
		t.Fatalf("normalized BlockedBy = %#v, want %#v", updated.BlockedBy, want)
	}

	updated = mustUpdate(t, store, updateInput{
		TaskID:          taskID.String(),
		AddBlockedBy:    []string{firstID.String(), secondID.String(), secondID.String()},
		RemoveBlockedBy: []string{secondID.String(), secondID.String()},
	})
	if want := []string{firstID.String()}; !reflect.DeepEqual(updated.BlockedBy, want) {
		t.Fatalf("second normalized BlockedBy = %#v, want %#v", updated.BlockedBy, want)
	}
}

func TestStoreBlockedTaskCannotTransitionToInProgress(t *testing.T) {
	blockerID := testUUID(1)
	taskID := testUUID(2)
	store := newStore(sequenceIDs(blockerID, taskID))
	mustCreate(t, store, validCreateInput("blocker"))
	input := validCreateInput("blocked")
	input.BlockedBy = []string{blockerID.String()}
	mustCreate(t, store, input)

	before := snapshotRecords(t, store)
	if _, _, err := store.update(updateInput{TaskID: taskID.String(), Status: statusPointer(StatusInProgress)}); err == nil {
		t.Fatal("store.update() transitioned a blocked task to in_progress")
	}
	assertRecordsUnchanged(t, store, before)
}

func TestStoreInProgressTaskCannotAcquireIncompleteBlocker(t *testing.T) {
	taskID := testUUID(1)
	blockerID := testUUID(2)
	store := newStore(sequenceIDs(taskID, blockerID))
	mustCreate(t, store, validCreateInput("target"))
	mustUpdate(t, store, updateInput{TaskID: taskID.String(), Status: statusPointer(StatusInProgress)})
	mustCreate(t, store, validCreateInput("incomplete blocker"))

	before := snapshotRecords(t, store)
	if _, _, err := store.update(updateInput{TaskID: taskID.String(), AddBlockedBy: []string{blockerID.String()}}); err == nil {
		t.Fatal("store.update() added an incomplete blocker to an in_progress task")
	}
	assertRecordsUnchanged(t, store, before)
}

func TestStoreCompletedBlockerPermitsLaterTransitionWithoutAutoStartingDependents(t *testing.T) {
	blockerID := testUUID(1)
	firstDependentID := testUUID(2)
	secondDependentID := testUUID(3)
	store := newStore(sequenceIDs(blockerID, firstDependentID, secondDependentID))
	mustCreate(t, store, validCreateInput("blocker"))
	firstInput := validCreateInput("first dependent")
	firstInput.BlockedBy = []string{blockerID.String()}
	mustCreate(t, store, firstInput)
	secondInput := validCreateInput("second dependent")
	secondInput.BlockedBy = []string{blockerID.String()}
	mustCreate(t, store, secondInput)

	mustUpdate(t, store, updateInput{TaskID: blockerID.String(), Status: statusPointer(StatusCompleted)})
	first, err := store.get(firstDependentID.String())
	if err != nil {
		t.Fatalf("store.get(first dependent) error = %v", err)
	}
	second, err := store.get(secondDependentID.String())
	if err != nil {
		t.Fatalf("store.get(second dependent) error = %v", err)
	}
	if first.Status != StatusPending || second.Status != StatusPending {
		t.Fatalf("completing blocker auto-started dependents: first=%q second=%q", first.Status, second.Status)
	}

	updated := mustUpdate(t, store, updateInput{TaskID: firstDependentID.String(), Status: statusPointer(StatusInProgress)})
	if updated.Status != StatusInProgress {
		t.Fatalf("dependent Status = %q, want %q", updated.Status, StatusInProgress)
	}
	second, err = store.get(secondDependentID.String())
	if err != nil {
		t.Fatalf("store.get(second dependent after transition) error = %v", err)
	}
	if second.Status != StatusPending {
		t.Fatalf("transitioning one dependent auto-started another: %q", second.Status)
	}
}

func TestStoreDeleteRemovesTaskAndInboundReferences(t *testing.T) {
	victimID := testUUID(1)
	firstDependentID := testUUID(2)
	secondDependentID := testUUID(3)
	store := newStore(sequenceIDs(victimID, firstDependentID, secondDependentID))
	mustCreate(t, store, validCreateInput("victim"))
	firstInput := validCreateInput("first dependent")
	firstInput.BlockedBy = []string{victimID.String()}
	mustCreate(t, store, firstInput)
	secondInput := validCreateInput("second dependent")
	secondInput.BlockedBy = []string{victimID.String()}
	mustCreate(t, store, secondInput)

	deleted, wasDeleted, err := store.update(updateInput{TaskID: victimID.String(), Status: statusPointer(StatusCommandDeleted)})
	if err != nil {
		t.Fatalf("store.update(delete) error = %v", err)
	}
	if !wasDeleted {
		t.Fatal("store.update(delete) deleted = false, want true")
	}
	if !reflect.DeepEqual(deleted, Task{}) {
		t.Fatalf("deleted task response = %#v, want zero Task", deleted)
	}
	if _, err := store.get(victimID.String()); err == nil {
		t.Fatal("deleted task remained in store")
	}
	for _, id := range []string{firstDependentID.String(), secondDependentID.String()} {
		remaining, err := store.get(id)
		if err != nil {
			t.Fatalf("store.get(%s) error = %v", id, err)
		}
		if len(remaining.BlockedBy) != 0 {
			t.Fatalf("inbound reference to deleted task remained on %s: %#v", id, remaining.BlockedBy)
		}
	}
}

func TestStoreUpdateFieldLimitsRejectAtomically(t *testing.T) {
	taskID := testUUID(1)
	store := newStore(sequenceIDs(taskID))
	input := validCreateInput("original")
	input.ActiveForm = "original active form"
	input.Metadata = json.RawMessage(`{"owner":"original"}`)
	mustCreate(t, store, input)

	tests := []struct {
		name  string
		input updateInput
	}{
		{
			name:  "subject",
			input: updateInput{TaskID: taskID.String(), Subject: stringPointer(strings.Repeat("s", maxSubjectBytes+1))},
		},
		{
			name:  "description",
			input: updateInput{TaskID: taskID.String(), Description: stringPointer(strings.Repeat("d", maxDescriptionBytes+1))},
		},
		{
			name:  "active form",
			input: updateInput{TaskID: taskID.String(), ActiveForm: stringPointer(strings.Repeat("a", maxActiveFormBytes+1))},
		},
		{
			name:  "metadata",
			input: updateInput{TaskID: taskID.String(), Metadata: metadataPointer(strings.Repeat("m", maxMetadataBytes+1))},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := snapshotRecords(t, store)
			if _, _, err := store.update(tt.input); err == nil {
				t.Fatal("store.update() accepted an oversized field")
			}
			assertRecordsUnchanged(t, store, before)
		})
	}
}

func TestStoreDependencyLimitUpdateRejectsAtomically(t *testing.T) {
	ids := idsFor(maxDependencies + 2)
	store := newStore(sequenceIDs(ids...))
	for i := 0; i < maxDependencies+1; i++ {
		mustCreate(t, store, validCreateInput("dependency"))
	}
	taskID := ids[maxDependencies+1]
	mustCreate(t, store, validCreateInput("target"))

	dependencies := make([]string, maxDependencies+1)
	for i := range dependencies {
		dependencies[i] = ids[i].String()
	}
	before := snapshotRecords(t, store)
	if _, _, err := store.update(updateInput{TaskID: taskID.String(), AddBlockedBy: dependencies}); err == nil {
		t.Fatal("store.update() accepted too many dependencies")
	}
	assertRecordsUnchanged(t, store, before)
}

func TestStoreAggregateLimitUpdateRejectsAtomically(t *testing.T) {
	ids := idsFor(127)
	store := newStore(sequenceIDs(ids...))
	for range ids {
		input := createInput{
			Subject:     "s",
			Description: strings.Repeat("d", maxDescriptionBytes),
		}
		mustCreate(t, store, input)
	}

	before := snapshotRecords(t, store)
	if _, _, err := store.update(updateInput{
		TaskID:   ids[0].String(),
		Metadata: metadataPointer(strings.Repeat("m", maxMetadataBytes)),
	}); err == nil {
		t.Fatal("store.update() exceeded maxStoreBytes")
	}
	assertRecordsUnchanged(t, store, before)
}
