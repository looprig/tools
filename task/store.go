package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/looprig/core/uuid"
)

const (
	maxTaskArgsBytes    = 64 << 10
	maxTasksPerLoop     = 256
	maxSubjectBytes     = 512
	maxDescriptionBytes = 16 << 10
	maxActiveFormBytes  = 512
	maxMetadataBytes    = 16 << 10
	maxDependencies     = 128
	maxStoreBytes       = 2 << 20
)

type idSource func() (uuid.UUID, error)

type createInput struct {
	Subject     string
	Description string
	ActiveForm  string
	BlockedBy   []string
	Metadata    json.RawMessage
}

type store struct {
	mu    sync.Mutex
	newID idSource
	tasks map[string]taskRecord
}

func newStore(source idSource) *store {
	if source == nil {
		source = uuid.New
	}
	return &store{
		newID: source,
		tasks: make(map[string]taskRecord),
	}
}

func (s *store) create(input createInput, requestBytes ...int) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.tasks) >= maxTasksPerLoop {
		return Task{}, errors.New("task store limit exceeded: too many tasks")
	}

	argsBytes := createInputBytes(input)
	if len(requestBytes) > 1 {
		return Task{}, errors.New("task create accepts at most one request-size value")
	}
	if len(requestBytes) == 1 {
		argsBytes = requestBytes[0]
	}
	if argsBytes < 0 || argsBytes > maxTaskArgsBytes {
		return Task{}, errors.New("task create request exceeds the argument limit")
	}

	dependencies, err := s.normalizeDependencies(input.BlockedBy)
	if err != nil {
		return Task{}, err
	}
	if strings.TrimSpace(input.Subject) == "" {
		return Task{}, errors.New("task subject is required")
	}
	if len(input.Subject) > maxSubjectBytes {
		return Task{}, errors.New("task subject exceeds the byte limit")
	}
	if strings.TrimSpace(input.Description) == "" {
		return Task{}, errors.New("task description is required")
	}
	if len(input.Description) > maxDescriptionBytes {
		return Task{}, errors.New("task description exceeds the byte limit")
	}
	if len(input.ActiveForm) > maxActiveFormBytes {
		return Task{}, errors.New("task active form exceeds the byte limit")
	}
	if len(input.Metadata) > maxMetadataBytes {
		return Task{}, errors.New("task metadata exceeds the byte limit")
	}

	source := s.newID
	if source == nil {
		source = uuid.New
	}
	id, err := source()
	if err != nil {
		return Task{}, fmt.Errorf("generate task ID: %w", err)
	}
	idString := id.String()
	if _, exists := s.tasks[idString]; exists {
		return Task{}, fmt.Errorf("task ID collision: %s", idString)
	}

	candidate := taskRecord{
		ID:          idString,
		Subject:     input.Subject,
		Description: input.Description,
		ActiveForm:  input.ActiveForm,
		Status:      StatusPending,
		BlockedBy:   dependencies,
		Metadata:    cloneRawMessage(input.Metadata),
	}
	if storeRecordBytes(s.tasks)+recordBytes(candidate) > maxStoreBytes {
		return Task{}, errors.New("task store limit exceeded: aggregate size")
	}

	s.tasks[idString] = candidate
	return taskFromRecord(candidate, s.tasks), nil
}

func (s *store) get(id string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	parsed, err := uuid.Parse(id)
	if err != nil {
		return Task{}, fmt.Errorf("invalid task ID: %w", err)
	}
	canonicalID := parsed.String()
	record, ok := s.tasks[canonicalID]
	if !ok {
		return Task{}, fmt.Errorf("unknown task ID: %s", canonicalID)
	}
	return taskFromRecord(record, s.tasks), nil
}

func (s *store) list() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]string, 0, len(s.tasks))
	for id := range s.tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	listed := make([]Task, 0, len(ids))
	for _, id := range ids {
		listed = append(listed, taskFromRecord(s.tasks[id], s.tasks))
	}
	return listed
}

func (s *store) normalizeDependencies(dependencies []string) ([]string, error) {
	if len(dependencies) > maxDependencies {
		return nil, errors.New("task has too many dependencies")
	}

	normalized := make([]string, 0, len(dependencies))
	seen := make(map[string]struct{}, len(dependencies))
	for _, dependency := range dependencies {
		parsed, err := uuid.Parse(dependency)
		if err != nil {
			return nil, fmt.Errorf("invalid task dependency %q: %w", dependency, err)
		}
		canonicalID := parsed.String()
		if _, ok := s.tasks[canonicalID]; !ok {
			return nil, fmt.Errorf("unknown task dependency: %s", canonicalID)
		}
		if _, duplicate := seen[canonicalID]; duplicate {
			continue
		}
		seen[canonicalID] = struct{}{}
		normalized = append(normalized, canonicalID)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func createInputBytes(input createInput) int {
	total := len(input.Subject) + len(input.Description) + len(input.ActiveForm) + len(input.Metadata)
	for _, dependency := range input.BlockedBy {
		total += len(dependency)
	}
	return total
}

func storeRecordBytes(records map[string]taskRecord) int {
	total := 0
	for _, record := range records {
		total += recordBytes(record)
	}
	return total
}

func recordBytes(record taskRecord) int {
	total := len(record.ID) + len(record.Subject) + len(record.Description) + len(record.ActiveForm) + len(record.Status) + len(record.Metadata)
	for _, dependency := range record.BlockedBy {
		total += len(dependency)
	}
	return total
}
