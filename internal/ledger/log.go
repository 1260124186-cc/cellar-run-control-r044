package ledger

import "encoding/json"

// Log is the in-memory append-only trajectory. It is safe for concurrent
// use only when an outer repository serializes access; the kernel itself
// performs no locking because it is committed together with the business
// snapshot under the repository's own transaction.
type Log struct {
	entries []Entry
	// index maps "type:id" entity keys to the global sequences that touch
	// the entity either as subject or through lineage.
	index map[string][]int64
	// versions maps "type:id" to entity-version -> global sequence,
	// enforcing idempotent appends for each subject.
	versions map[string]map[int64]int64
}

// New returns an empty, initialized log.
func New() *Log {
	return &Log{
		entries:  []Entry{},
		index:    map[string][]int64{},
		versions: map[string]map[int64]int64{},
	}
}

// Build validates and indexes a log reconstructed from persisted entries.
// Sequences must be gapless starting at 1 and each idempotency key may
// appear at most once.
func Build(entries []Entry) (*Log, error) {
	log := New()
	if len(entries) == 0 {
		return log, nil
	}
	log.entries = make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.Sequence != int64(len(log.entries)+1) {
			return nil, corruptError("trajectory sequences must be gapless and begin at 1")
		}
		if err := entry.validatePersisted(); err != nil {
			return nil, err
		}
		entry.normalize()
		if err := log.indexEntry(entry); err != nil {
			return nil, err
		}
		log.entries = append(log.entries, entry)
	}
	return log, nil
}

// EnsureInitialized fills an uninitialized log (for example one decoded
// from a snapshot written before the trajectory existed).
func (l *Log) EnsureInitialized() {
	if l == nil {
		return
	}
	if l.entries == nil {
		l.entries = []Entry{}
	}
	if l.index == nil {
		l.index = map[string][]int64{}
	}
	if l.versions == nil {
		l.versions = map[string]map[int64]int64{}
	}
}

// Len returns the number of committed entries.
func (l *Log) Len() int {
	l.EnsureInitialized()
	return len(l.entries)
}

// LastSequence returns the highest assigned sequence, or 0 when empty.
func (l *Log) LastSequence() int64 {
	l.EnsureInitialized()
	if len(l.entries) == 0 {
		return 0
	}
	return l.entries[len(l.entries)-1].Sequence
}

// Append adds an entry under its idempotency key (subject type, subject id,
// entity version). Re-processing the same version returns the already
// recorded entry when the payload matches; a different payload is a
// conflict that aborts the surrounding transaction.
func (l *Log) Append(candidate Entry) (Entry, error) {
	l.EnsureInitialized()
	if err := candidate.validateForAppend(); err != nil {
		return Entry{}, err
	}
	candidate.normalize()

	subjectKey := entityKey(candidate.Subject.Type, candidate.Subject.ID)
	if record, ok := l.versions[subjectKey][candidate.EntityVersion]; ok {
		existing := l.entries[record-1]
		if existing.sameBusiness(candidate) {
			return existing.Clone(), nil
		}
		return Entry{}, &Error{
			Kind:    KindConflict,
			Message: "entity version was already recorded with a different trajectory entry",
			Field:   "entity_version",
		}
	}

	candidate.Sequence = int64(len(l.entries) + 1)
	if err := l.indexEntry(candidate); err != nil {
		return Entry{}, err
	}
	l.entries = append(l.entries, candidate)
	return candidate.Clone(), nil
}

// indexEntry adds the candidate to the lineage and version indexes.
func (l *Log) indexEntry(entry Entry) error {
	subjectKey := entityKey(entry.Subject.Type, entry.Subject.ID)
	if _, ok := l.versions[subjectKey]; !ok {
		l.versions[subjectKey] = map[int64]int64{}
	}
	if _, dup := l.versions[subjectKey][entry.EntityVersion]; dup {
		return corruptError("trajectory contains two entries for the same entity version")
	}
	l.versions[subjectKey][entry.EntityVersion] = entry.Sequence

	l.touchIndex(entry.Subject, entry.Sequence)
	for _, ref := range entry.Lineage {
		l.touchIndex(ref, entry.Sequence)
	}
	return nil
}

func (l *Log) touchIndex(ref Ref, sequence int64) {
	key := entityKey(ref.Type, ref.ID)
	l.index[key] = append(l.index[key], sequence)
}

// EntryAt returns a copy of the entry at a global sequence (1-based).
func (l *Log) EntryAt(sequence int64) (Entry, bool) {
	l.EnsureInitialized()
	if sequence < 1 || int(sequence) > len(l.entries) {
		return Entry{}, false
	}
	return l.entries[sequence-1].Clone(), true
}

// Walk visits matching entries in global sequence order, invoking visitor
// with a copy of each entry. The walk stops at the first visitor error.
func (l *Log) Walk(filter Filter, visitor func(Entry) error) error {
	l.EnsureInitialized()
	for _, entry := range l.entries {
		if !filter.matches(entry) {
			continue
		}
		if err := visitor(entry.Clone()); err != nil {
			return err
		}
	}
	return nil
}

// Query returns one stable page of matching entries in global sequence
// order plus the cursor to continue after the page.
func (l *Log) Query(filter Filter, limit int, afterSequence int64) Page {
	l.EnsureInitialized()
	if limit < 1 {
		limit = DefaultPageLimit
	}
	if limit > MaxPageLimit {
		limit = MaxPageLimit
	}
	if afterSequence < 0 {
		afterSequence = 0
	}

	page := Page{
		Entries: []Entry{},
		Limit:   limit,
	}
	for _, entry := range l.entries {
		if entry.Sequence <= afterSequence || !filter.matches(entry) {
			continue
		}
		if len(page.Entries) >= limit {
			page.HasMore = true
			break
		}
		page.Entries = append(page.Entries, entry.Clone())
	}
	if page.HasMore && len(page.Entries) > 0 {
		page.NextCursor = EncodeCursor(page.Entries[len(page.Entries)-1].Sequence)
	}
	return page
}

// Clone returns a fully independent deep copy of the log.
func (l *Log) Clone() *Log {
	l.EnsureInitialized()
	cloned := New()
	cloned.entries = make([]Entry, 0, len(l.entries))
	for _, entry := range l.entries {
		cloned.entries = append(cloned.entries, entry.Clone())
	}
	for key, sequences := range l.index {
		cloned.index[key] = append([]int64(nil), sequences...)
	}
	for key, versions := range l.versions {
		copied := make(map[int64]int64, len(versions))
		for version, sequence := range versions {
			copied[version] = sequence
		}
		cloned.versions[key] = copied
	}
	return cloned
}

// MarshalJSON persists the trajectory as a plain gapless entry array so the
// on-disk format stays simple and human-auditable; indexes are rebuilt on
// load.
func (l Log) MarshalJSON() ([]byte, error) {
	l.EnsureInitialized()
	return json.Marshal(l.entries)
}

// UnmarshalJSON rebuilds and validates the log from persisted entries.
func (l *Log) UnmarshalJSON(raw []byte) error {
	var entries []Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return err
	}
	built, err := Build(entries)
	if err != nil {
		return err
	}
	*l = *built
	return nil
}

// validatePersisted validates an entry already carrying its assigned
// sequence while loading or rebuilding the log.
func (e Entry) validatePersisted() error {
	if e.Sequence < 1 {
		return corruptError("trajectory sequence must be positive")
	}
	copy := e
	copy.Sequence = 0
	return copy.validateForAppend()
}

func entityKey(entityType EntityType, id string) string {
	return string(entityType) + ":" + id
}
