package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"q-agent/internal/learning"
)

func TestLearningEraseUnreadablePrivatePartition(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{"malformed", []byte(`{"snapshot":{"private":"sensitive draft"`), errLearningPartitionCorrupt},
		{"oversized", bytes.Repeat([]byte("private"), maxLearningPartitionBytes/7+1), ErrLearningPartitionFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := NewLearningRepository(t.TempDir(), nil)
			topic := int64(7)
			scope := learning.Scope{Platform: "telegram", ChatID: -100, TopicID: &topic, Visibility: learning.GroupChat}
			key, err := learningPartitionKey(learning.Partition{Scope: &scope})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(repo.dir, 0700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(repo.dir, key+".json")
			backup := file + ".bak"
			neighbor := filepath.Join(repo.dir, "unrelated.json.bak")
			for _, p := range []string{file, backup, neighbor} {
				if err := os.WriteFile(p, tc.data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC()
			request := rand.Text()
			if _, err := repo.DeletePrivateLearning(ctx, scope, "conversation-00000000000000000000000000000000", request, now); !errors.Is(err, tc.want) {
				t.Fatalf("single-candidate request must preserve unreadable neighbors: %v", err)
			}
			if data, err := os.ReadFile(file); err != nil || !bytes.Equal(data, tc.data) {
				t.Fatalf("single-candidate request changed the partition: %v", err)
			}
			receipt, err := repo.DeletePrivateLearning(ctx, scope, "", request, now)
			if err != nil || receipt.ID != request || !receipt.UnreadablePartition {
				t.Fatalf("whole-topic receipt: %+v, %v", receipt, err)
			}
			if _, err := os.Stat(backup); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("controlled backup survived: %v", err)
			}
			if data, err := os.ReadFile(neighbor); err != nil || !bytes.Equal(data, tc.data) {
				t.Fatalf("unrelated backup was changed: %v", err)
			}
			reopened, err := repo.load(key)
			if err != nil || len(reopened.Snapshot.Frontier) != 0 || len(reopened.Deletions) != 1 ||
				!privateLearningAfter(reopened, now.Add(-time.Hour)).After(now) {
				t.Fatalf("erasure cutoff after reopening: %+v, %v", reopened.Deletions, err)
			}
			again, err := repo.DeletePrivateLearning(ctx, scope, "", request, now.Add(time.Minute))
			if err != nil || receipt != again {
				t.Fatalf("erasure replay changed receipt: %+v, %v", again, err)
			}
		})
	}
}
