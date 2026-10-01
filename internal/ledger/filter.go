package ledger

import (
	"encoding/base64"
	"fmt"
	"time"
)

const (
	// DefaultPageLimit applies when the caller does not request a size.
	DefaultPageLimit = 100
	// MaxPageLimit bounds a single page response.
	MaxPageLimit = 500
)

// Filter narrows a trajectory read. Every field is optional: the zero
// value reads the complete log in global sequence order.
type Filter struct {
	// EntityType and EntityID must be provided together. When set, only
	// entries where the entity is the subject or a lineage reference are
	// returned, covering every side of the production bloodline.
	EntityType EntityType
	EntityID   string
	// From keeps entries occurring at or after the given time. It is a
	// time cursor; pagination continuity still comes from sequence order,
	// so equal timestamps never drop or reorder rows.
	From *time.Time
}

func (f Filter) matches(entry Entry) bool {
	if f.EntityType != "" || f.EntityID != "" {
		if f.EntityType == "" || f.EntityID == "" || !f.touchesEntity(entry) {
			return false
		}
	}
	if f.From != nil && entry.OccurredAt.Before((*f.From).UTC()) {
		return false
	}
	return true
}

func (f Filter) touchesEntity(entry Entry) bool {
	wanted := entityKey(f.EntityType, f.EntityID)
	if entityKey(entry.Subject.Type, entry.Subject.ID) == wanted {
		return true
	}
	for _, ref := range entry.Lineage {
		if entityKey(ref.Type, ref.ID) == wanted {
			return true
		}
	}
	return false
}

// Page is one stable slice of a paged trajectory read.
type Page struct {
	Entries    []Entry `json:"entries"`
	NextCursor string  `json:"next_cursor,omitempty"`
	HasMore    bool    `json:"has_more"`
	Limit      int     `json:"limit"`
}

// EncodeCursor renders a sequence position as an opaque page cursor.
// The cursor points at the last returned entry; the next page begins
// strictly after it.
func EncodeCursor(sequence int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("seq:%d", sequence)))
}

// DecodeCursor parses an opaque page cursor back into a sequence position.
func DecodeCursor(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, fmt.Errorf("cursor must be a valid trajectory cursor")
	}
	var sequence int64
	if _, err := fmt.Sscanf(string(raw), "seq:%d", &sequence); err != nil {
		return 0, fmt.Errorf("cursor must be a valid trajectory cursor")
	}
	if sequence < 0 {
		return 0, fmt.Errorf("cursor must be a valid trajectory cursor")
	}
	return sequence, nil
}
