package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"q-agent/internal/platform"
	"q-agent/internal/redaction"
)

var ErrStaleDigest = errors.New("memory changed during digest")

func (s *Store) recoverMemory() error {
	entries, err := os.ReadDir(filepath.Join(s.dir, "memory"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		key := strings.TrimSuffix(entry.Name(), ".json")
		mem, err := s.loadMemory(key)
		if err != nil {
			// A single corrupt chat file must not block the whole bot.
			// Quarantine JSON syntax failures aside and start that chat
			// fresh; version and I/O errors still fail startup.
			var syntax *json.SyntaxError
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &syntax) || errors.As(err, &typeErr) {
				if qerr := s.quarantineMemory(key); qerr != nil {
					return qerr
				}
				continue
			}
			return err
		}
		if err := s.recoverExchange(key, &mem); err != nil {
			return err
		}
		previous, _ := json.Marshal(mem)
		s.boundMemory(&mem, s.memoryLimits.MaxBytes)
		s.boundPending(key, &mem, mem.Route)
		for len(mem.Notices) > MaxMemoryNotices {
			mem.Notices = mem.Notices[1:]
		}
		current, _ := json.Marshal(mem)
		if !bytes.Equal(previous, current) {
			// Notice-only trimming must not invalidate in-flight digests,
			// so only bump Revision when bounds/source state changed.
			prevMem := ChatMemory{}
			_ = json.Unmarshal(previous, &prevMem)
			boundsChanged := prevMem.Cursor != mem.Cursor ||
				prevMem.NextSequence != mem.NextSequence ||
				prevMem.Epoch != mem.Epoch ||
				len(prevMem.Pending) != len(mem.Pending) ||
				prevMem.Bytes() != mem.Bytes() ||
				prevMem.Loss != mem.Loss ||
				prevMem.Overflow != mem.Overflow
			if boundsChanged {
				mem.Revision++
			}
			if err := s.writeMemory(key, mem); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) recoverExchange(key string, mem *ChatMemory) error {
	if mem.Intent == nil {
		return nil
	}
	if err := s.atomic(s.path(mem.Intent.Key), mem.Intent.Conversation); err != nil {
		return err
	}
	mem.Intent = nil
	return s.writeMemory(key, *mem)
}

// quarantineMemory renames a corrupt per-chat memory file aside so one bad
// chat cannot block startup for all chats. The chat restarts fresh; the
// quarantined copy keeps the original bytes for manual inspection.
func (s *Store) quarantineMemory(key string) error {
	src := s.memoryPath(key)
	dst := filepath.Join(s.dir, "memory", key+".corrupt-"+time.Now().UTC().Format("20060102T150405Z999999999")+".json")
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	delete(s.memories, key)
	return SyncDir(filepath.Join(s.dir, "memory"))
}

// recoverHistory runs before reads and every history mutation. A reset must
// follow recovery so replay cannot resurrect history that was already reset.
func (s *Store) recoverHistory() error {
	keys := make([]string, 0, len(s.memories))
	for key, mem := range s.memories {
		if mem.Intent != nil {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		mem, err := s.loadMemory(key)
		if err != nil {
			return err
		}
		if err := s.recoverExchange(key, &mem); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) cleanConversation(c Conversation) Conversation {
	c.Pairs = append([]Pair(nil), c.Pairs...)
	for i := range c.Pairs {
		c.Pairs[i].User = s.policy.Text(redaction.Persistence, c.Pairs[i].User)
		c.Pairs[i].Answer = s.policy.Text(redaction.Persistence, c.Pairs[i].Answer)
		c.Pairs[i].Steers = append([]string(nil), c.Pairs[i].Steers...)
		for j, steer := range c.Pairs[i].Steers {
			c.Pairs[i].Steers[j] = s.policy.Text(redaction.Persistence, steer)
		}
	}
	c.Bound()
	return c
}

func sourceTranscript(p Pair) string {
	var b strings.Builder
	b.WriteString("user: ")
	b.WriteString(p.User)
	for _, steer := range p.Steers {
		b.WriteString("\nsteer: ")
		b.WriteString(steer)
	}
	b.WriteString("\nassistant: ")
	b.WriteString(p.Answer)
	b.WriteByte('\n')
	return b.String()
}

const sourceOmission = "\n[source middle omitted: event exceeded 256 KiB; head/tail retained]\n"

func retainHeadTail(text string, n int) string {
	head := UTF8Prefix(text, n/2)
	start := len(text) - (n - n/2)
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return head + sourceOmission + text[start:]
}

// truncateSourceTranscript fits a base transcript into MaxSourceBytes for the
// given event identity. It returns the stored transcript, the original
// encoded size, and the final encoded size for loss accounting.
func truncateSourceTranscript(base string, id string, topic int64, at time.Time, seq uint64) (string, int, int) {
	event := SourceEvent{Sequence: seq, ID: id, Topic: topic, At: at, Transcript: base}
	original := encodedSize(event)
	if original <= MaxSourceBytes {
		return base, original, original
	}
	low, high := 0, len(base)
	for low < high {
		mid := (low + high + 1) / 2
		event.Transcript = retainHeadTail(base, mid)
		if encodedSize(event) <= MaxSourceBytes {
			low = mid
		} else {
			high = mid - 1
		}
	}
	event.Transcript = retainHeadTail(base, low)
	return event.Transcript, original, encodedSize(event)
}

// CommitExchange first durably commits the source and history intent in one
// memory write. Recovery only applies the intent; it never re-enqueues a source
// or reruns agent work. Eligibility is determined at completion, after forget.
//
// CPU-heavy transcript redaction/truncation happens before the store lock so a
// huge exchange does not stall other chats; the lock still covers the sequence
// allocation and the two synced writes required for durability.
func (s *Store) CommitExchange(route platform.Message, sourceID string, c Conversation, at time.Time) error {
	if len(c.Pairs) == 0 || sourceID == "" {
		return fmt.Errorf("exchange requires source identity and transcript")
	}
	// Immutable policy work outside the global lock.
	cleaned := s.cleanConversation(c)
	base := strings.ToValidUTF8(s.policy.Text(redaction.Persistence, sourceTranscript(c.Pairs[len(c.Pairs)-1])), "�")
	at = at.UTC()
	// Pre-truncate with a placeholder sequence; the in-lock check below only
	// adjusts for a few digits of sequence growth.
	preTruncated, _, _ := truncateSourceTranscript(base, sourceID, route.TopicID, at, 0)
	s.mu.Lock()
	defer s.mu.Unlock()
	key := route.WorkspaceKey()
	mem, err := s.loadMemory(key)
	if err != nil {
		return err
	}
	if err := s.recoverExchange(key, &mem); err != nil {
		return err
	}
	if mem.LastSourceID == sourceID {
		return nil
	}
	// Re-validate with the real sequence; usually a no-op after pre-truncation.
	mem.NextSequence++
	stored := preTruncated
	original := encodedSize(SourceEvent{Sequence: mem.NextSequence, ID: sourceID, Topic: route.TopicID, At: at, Transcript: base})
	final := encodedSize(SourceEvent{Sequence: mem.NextSequence, ID: sourceID, Topic: route.TopicID, At: at, Transcript: stored})
	if final > MaxSourceBytes {
		// Sequence growth pushed a borderline event over; re-truncate.
		// Rare: pre-truncation already fit for seq 0.
		var orig int
		stored, orig, final = truncateSourceTranscript(base, sourceID, route.TopicID, at, mem.NextSequence)
		original = orig
	}
	event := SourceEvent{Sequence: mem.NextSequence, ID: sourceID, Topic: route.TopicID, At: at, Transcript: stored}
	if original > MaxSourceBytes {
		mem.Loss.TruncatedEvents++
		// Count gross encoded bytes removed, not net saved: the retained
		// head/tail marker itself occupies encoded space.
		omissionEncoded := encodedSize(sourceOmission) - 2
		if omissionEncoded < 0 {
			omissionEncoded = 0
		}
		mem.Loss.TruncatedBytes += uint64(original - final + omissionEncoded)
	}
	mem.Pending = append(mem.Pending, event)
	mem.Route = platform.Message{Platform: route.Platform, ChatID: route.ChatID, UserID: route.UserID}
	s.boundPending(key, &mem, mem.Route)
	mem.LastSourceID = sourceID
	mem.Intent = &ExchangeIntent{Key: route.ConversationKey(), Conversation: cleaned}
	if err := s.writeMemory(key, mem); err != nil {
		return err
	}
	return s.recoverExchange(key, &mem)
}

func encodedSize(value any) int {
	data, _ := json.Marshal(value)
	return len(data)
}

// SnapshotDigest returns independently owned data. Model requests must happen
// after this method releases the lock.
func (s *Store) SnapshotDigest(key string) (ChatMemory, error) { return s.LoadMemory(key) }

// CommitDigest conditionally acknowledges exactly a prefix of the snapshot.
// Appends survive; forget, eviction, and overlapping digest commits invalidate it.
// Notes, cursor, journal removal, and delivery intent share one atomic write.
func (s *Store) CommitDigest(key string, snapshot, result ChatMemory, count int, route platform.Message, parts []string, at time.Time, maxBytes int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	mem, err := s.loadMemory(key)
	if err != nil {
		return err
	}
	if mem.Epoch != snapshot.Epoch || mem.Revision != snapshot.Revision || mem.Cursor != snapshot.Cursor {
		return ErrStaleDigest
	}
	if count <= 0 || count > len(snapshot.Pending) || count > len(mem.Pending) {
		return ErrStaleDigest
	}
	for i := 0; i < count; i++ {
		a, b := mem.Pending[i], snapshot.Pending[i]
		if a.ID != b.ID || a.Sequence != b.Sequence || a.Topic != b.Topic || a.Transcript != b.Transcript || !a.At.Equal(b.At) {
			return ErrStaleDigest
		}
	}
	mem.Entries = append([]MemoryEntry(nil), result.Entries...)
	mem.Loss.DroppedNotes += result.Loss.DroppedNotes - snapshot.Loss.DroppedNotes
	mem.Loss.DroppedNoteBytes += result.Loss.DroppedNoteBytes - snapshot.Loss.DroppedNoteBytes
	s.boundMemory(&mem, maxBytes)
	first, last := mem.Pending[0].Sequence, mem.Pending[count-1].Sequence
	mem.Cursor = last
	mem.Pending = mem.Pending[count:]
	mem.Revision++
	mem.LastDigestAt = at.UTC()
	mem.Overflow = false
	mem.Retry = MemoryRetry{Work: len(mem.Pending) > 0}
	s.addMemoryNotice(&mem, route, fmt.Sprintf("memory:%s:digest:%d:%d:%d", key, mem.Epoch, first, last), parts)
	return s.writeMemory(key, mem)
}

// RetryMemory persists digest scheduling independently of prompt admission.
// Failed storage writes are also tracked by the runtime until storage recovers.
func (s *Store) RetryMemory(key string, epoch uint64, retry MemoryRetry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	mem, err := s.loadMemory(key)
	if err != nil {
		return err
	}
	if mem.Epoch != epoch {
		return ErrStaleDigest
	}
	mem.Retry = retry
	return s.writeMemory(key, mem)
}

func (s *Store) addMemoryNotice(mem *ChatMemory, route platform.Message, id string, parts []string) {
	if len(parts) == 0 {
		return
	}
	route.Text = ""
	parts = append([]string(nil), parts...)
	for i := range parts {
		parts[i] = s.policy.Text(redaction.Remote, parts[i])
	}
	mem.Notices = append(mem.Notices, Reply{ID: id, Route: route, Parts: parts})
	// Delivery can stall while digests succeed; bound the durable outbox so
	// one chat cannot grow its memory file without bound. Keep newest.
	for len(mem.Notices) > MaxMemoryNotices {
		mem.Notices = mem.Notices[1:]
	}
}

func (s *Store) pendingMemoryReply() (Reply, bool) {
	keys := make([]string, 0, len(s.memories))
	for key, mem := range s.memories {
		if len(mem.Notices) > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return Reply{}, false
	}
	key := keys[0]
	for _, candidate := range keys {
		if candidate > s.memoryLastKey {
			key = candidate
			break
		}
	}
	r := s.memories[key].Notices[0]
	r.Parts = append([]string(nil), r.Parts...)
	return r, true
}

func (s *Store) updateMemoryReply(id string, delivered bool) error {
	for key, cached := range s.memories {
		if len(cached.Notices) == 0 || cached.Notices[0].ID != id {
			continue
		}
		mem, err := s.loadMemory(key)
		if err != nil {
			return err
		}
		if len(mem.Notices) == 0 || mem.Notices[0].ID != id {
			return fmt.Errorf("memory outbox changed")
		}
		if delivered {
			mem.Notices[0].Next++
			if mem.Notices[0].Next >= len(mem.Notices[0].Parts) {
				mem.Notices = mem.Notices[1:]
			}
		} else if len(mem.Notices) > 1 {
			mem.Notices = append(mem.Notices[1:], mem.Notices[0])
		}
		if err := s.writeMemory(key, mem); err != nil {
			return err
		}
		s.memoryLastKey = key
		return nil
	}
	return fmt.Errorf("memory outbox changed")
}

func (s *Store) boundPending(key string, mem *ChatMemory, route platform.Message) {
	overflow := false
	size := mem.PendingBytes()
	for len(mem.Pending) > 0 && size > s.memoryLimits.MaxPendingBytes {
		removed := encodedSize(mem.Pending[0])
		mem.Loss.LostEvents++
		mem.Loss.LostBytes += uint64(removed)
		mem.Pending = mem.Pending[1:]
		size -= removed + 1 // the event and its separating comma
		if len(mem.Pending) == 0 {
			size = 0
		}
		overflow = true
	}
	if overflow {
		mem.Revision++ // an in-flight digest may reference an evicted source
		if !mem.Overflow {
			s.addMemoryNotice(mem, route, fmt.Sprintf("memory:%s:overflow:%d:%d", key, mem.Epoch, mem.NextSequence), []string{"Memory source capacity reached. Oldest pending exchanges were discarded; chat continues. See /memory for loss counters."})
		}
		mem.Overflow = true
	}
	// An episode ends when the journal drains. Without this, evict-all leaves
	// Overflow stuck true and suppresses the next episode's warning.
	if len(mem.Pending) == 0 {
		mem.Overflow = false
	}
}
