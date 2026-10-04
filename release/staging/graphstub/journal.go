package graphstub

import (
	"sync"
	"time"
)

// JournalCapacity bounds the in-memory request journal. The oldest entry is
// overwritten once it is full.
const JournalCapacity = 10_000

// maxJournalField bounds every caller-supplied string kept in an entry, so a
// full journal stays small whatever the requests contained.
const maxJournalField = 128

// Journal entry kinds.
const (
	KindGraph   = "graph"
	KindControl = "control"
	KindWebhook = "webhook"
	KindRefused = "refused"
)

// Entry records one request or webhook delivery. It never holds a request or
// response body, a token or a secret; text is kept only as its SHA-256.
type Entry struct {
	Seq               uint64    `json:"seq"`
	Time              time.Time `json:"time"`
	Kind              string    `json:"kind"`
	Method            string    `json:"method,omitempty"`
	Route             string    `json:"route"`
	Status            int       `json:"status"`
	PhoneNumberID     string    `json:"phone_number_id,omitempty"`
	BusinessAccountID string    `json:"business_account_id,omitempty"`
	MessageID         string    `json:"message_id,omitempty"`
	MessageType       string    `json:"message_type,omitempty"`
	To                string    `json:"to,omitempty"`
	TextSHA256        string    `json:"text_sha256,omitempty"`
	MediaID           string    `json:"media_id,omitempty"`
	TemplateName      string    `json:"template_name,omitempty"`
	DeliveryStatus    string    `json:"delivery_status,omitempty"`
	Fault             bool      `json:"fault,omitempty"`
	Error             string    `json:"error,omitempty"`
}

type journal struct {
	mu      sync.Mutex
	entries []Entry
	start   int
	seq     uint64
}

func newJournal() *journal {
	return &journal{entries: make([]Entry, 0, 64)}
}

func (j *journal) add(entry Entry) uint64 {
	for _, field := range []*string{
		&entry.Method, &entry.Route, &entry.PhoneNumberID, &entry.BusinessAccountID,
		&entry.MessageID, &entry.MessageType, &entry.To, &entry.MediaID,
		&entry.TemplateName, &entry.DeliveryStatus, &entry.Error,
	} {
		if len(*field) > maxJournalField {
			*field = (*field)[:maxJournalField]
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seq++
	entry.Seq = j.seq
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}
	if len(j.entries) < JournalCapacity {
		j.entries = append(j.entries, entry)
	} else {
		j.entries[j.start] = entry
		j.start = (j.start + 1) % JournalCapacity
	}
	return entry.Seq
}

// since returns up to limit entries with Seq > after, oldest first, and the
// latest sequence number assigned so far.
func (j *journal) since(after uint64, limit int) ([]Entry, uint64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	result := make([]Entry, 0, min(limit, len(j.entries)))
	for index := 0; index < len(j.entries) && len(result) < limit; index++ {
		entry := j.entries[(j.start+index)%len(j.entries)]
		if entry.Seq > after {
			result = append(result, entry)
		}
	}
	return result, j.seq
}

func (j *journal) reset() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = j.entries[:0]
	j.start = 0
}
