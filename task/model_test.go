package task

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestStatusValid(t *testing.T) {
	tests := []struct {
		name   string
		status Status
		want   bool
	}{
		{name: "pending", status: StatusPending, want: true},
		{name: "in progress", status: StatusInProgress, want: true},
		{name: "completed", status: StatusCompleted, want: true},
		{name: "deleted", status: Status("deleted"), want: false},
		{name: "unknown", status: Status("unknown"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.status.valid(); got != tt.want {
				t.Errorf("Status(%q).valid() = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestCloneRecordDeepCopiesDependenciesAndMetadata(t *testing.T) {
	original := taskRecord{
		ID:        "task-1",
		Subject:   "subject",
		Status:    StatusPending,
		BlockedBy: []string{"dependency-1", "dependency-2"},
		Metadata:  json.RawMessage(`{"component":"parser"}`),
	}

	cloned := cloneRecord(original)
	if !reflect.DeepEqual(cloned, original) {
		t.Fatalf("cloneRecord() = %#v, want %#v", cloned, original)
	}

	cloned.BlockedBy[0] = "changed-dependency"
	cloned.Metadata[0] = '['
	if original.BlockedBy[0] != "dependency-1" {
		t.Fatalf("mutating cloned BlockedBy changed original: %#v", original.BlockedBy)
	}
	if got := string(original.Metadata); got != `{"component":"parser"}` {
		t.Fatalf("mutating cloned Metadata changed original: %q", got)
	}

	clonedMetadata := string(cloned.Metadata)
	original.BlockedBy[1] = "changed-original-dependency"
	original.Metadata[1] = 'X'
	if cloned.BlockedBy[1] != "dependency-2" {
		t.Fatalf("mutating original BlockedBy changed clone: %#v", cloned.BlockedBy)
	}
	if got := string(cloned.Metadata); got != clonedMetadata {
		t.Fatalf("mutating original Metadata changed clone: %q", got)
	}
}

func TestTaskFromRecordDerivesBlocksWithoutPersistingIt(t *testing.T) {
	target := taskRecord{
		ID:        "task-target",
		Subject:   "target",
		Status:    StatusPending,
		BlockedBy: []string{"task-prerequisite"},
	}
	records := map[string]taskRecord{
		target.ID: {
			ID:        target.ID,
			Subject:   target.Subject,
			Status:    target.Status,
			BlockedBy: append([]string(nil), target.BlockedBy...),
		},
		"task-dependent-b": {
			ID:        "task-dependent-b",
			Subject:   "dependent b",
			Status:    StatusPending,
			BlockedBy: []string{target.ID},
		},
		"task-dependent-a": {
			ID:        "task-dependent-a",
			Subject:   "dependent a",
			Status:    StatusPending,
			BlockedBy: []string{"other-task", target.ID},
		},
		"task-unrelated": {
			ID:        "task-unrelated",
			Subject:   "unrelated",
			Status:    StatusPending,
			BlockedBy: []string{"other-task"},
		},
	}

	got := taskFromRecord(target, records)
	wantBlocks := []string{"task-dependent-a", "task-dependent-b"}
	if !reflect.DeepEqual(got.Blocks, wantBlocks) {
		t.Fatalf("Task.Blocks = %#v, want %#v", got.Blocks, wantBlocks)
	}
	if !reflect.DeepEqual(records[target.ID].BlockedBy, target.BlockedBy) {
		t.Fatalf("taskFromRecord mutated stored target record: %#v", records[target.ID])
	}
	if _, ok := reflect.TypeOf(taskRecord{}).FieldByName("Blocks"); ok {
		t.Fatal("taskRecord must not persist derived Blocks")
	}
}

func TestTaskFromRecordReturnedValuesAreDefensive(t *testing.T) {
	record := taskRecord{
		ID:        "task-target",
		Subject:   "target",
		Status:    StatusPending,
		BlockedBy: []string{"task-prerequisite"},
		Metadata:  json.RawMessage(`{"component":"parser"}`),
	}
	dependent := taskRecord{
		ID:        "task-dependent",
		Subject:   "dependent",
		Status:    StatusPending,
		BlockedBy: []string{record.ID},
	}
	records := map[string]taskRecord{
		record.ID:    record,
		dependent.ID: dependent,
	}

	got := taskFromRecord(record, records)
	got.BlockedBy[0] = "changed-prerequisite"
	got.Blocks[0] = "changed-dependent"
	got.Metadata[0] = '['

	if got := records[record.ID].BlockedBy; !reflect.DeepEqual(got, []string{"task-prerequisite"}) {
		t.Fatalf("mutating Task.BlockedBy changed stored record: %#v", got)
	}
	if got := records[dependent.ID].BlockedBy; !reflect.DeepEqual(got, []string{record.ID}) {
		t.Fatalf("mutating Task.Blocks changed stored dependency record: %#v", got)
	}
	if got := string(records[record.ID].Metadata); got != `{"component":"parser"}` {
		t.Fatalf("mutating Task.Metadata changed stored record: %q", got)
	}
}

func TestTaskFromRecordMetadataJSON(t *testing.T) {
	tests := []struct {
		name         string
		metadata     json.RawMessage
		wantMetadata json.RawMessage
	}{
		{
			name:         "nil metadata omitted",
			metadata:     nil,
			wantMetadata: nil,
		},
		{
			name:         "canonical non-empty metadata retained",
			metadata:     json.RawMessage(`{"component":"parser","priority":1}`),
			wantMetadata: json.RawMessage(`{"component":"parser","priority":1}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := taskRecord{
				ID:       "task-1",
				Subject:  "subject",
				Status:   StatusPending,
				Metadata: tt.metadata,
			}
			got := taskFromRecord(record, map[string]taskRecord{record.ID: record})

			if !bytes.Equal(got.Metadata, tt.wantMetadata) {
				t.Fatalf("Task.Metadata = %s, want %s", got.Metadata, tt.wantMetadata)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("json.Marshal(Task) error = %v", err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &object); err != nil {
				t.Fatalf("json.Unmarshal(Task) error = %v", err)
			}
			if tt.metadata == nil {
				if got.Metadata != nil {
					t.Fatalf("nil metadata became non-nil: %s", got.Metadata)
				}
				if _, ok := object["metadata"]; ok {
					t.Fatalf("encoded Task unexpectedly contains metadata: %s", encoded)
				}
			} else {
				metadata, ok := object["metadata"]
				if !ok {
					t.Fatalf("encoded Task omits non-empty metadata: %s", encoded)
				}
				if !bytes.Equal(metadata, tt.wantMetadata) {
					t.Fatalf("encoded metadata = %s, want %s", metadata, tt.wantMetadata)
				}
			}
		})
	}
}
