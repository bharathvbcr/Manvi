package lessons

import (
	"errors"
	"fmt"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/artifacts"
)

// unsyncedStore publishes every write and answers with the artifact beside
// ErrPublishedUnsynced, as artifacts.Store does when a directory sync fails.
type unsyncedStore struct {
	existing *artifacts.Artifact
	creates  int
	updates  int
}

func (s *unsyncedStore) Get(name string) (*artifacts.Artifact, error) {
	if s.existing == nil {
		return nil, fmt.Errorf("artifacts: artifact %q %w", name, artifacts.ErrNotFound)
	}
	return s.existing, nil
}

func (s *unsyncedStore) Create(name, content string, meta artifacts.Metadata) (*artifacts.Artifact, error) {
	s.creates++
	return &artifacts.Artifact{Name: name, Content: content, Metadata: meta, Revision: 1},
		fmt.Errorf("artifacts: %s: %w", name, artifacts.ErrPublishedUnsynced)
}

func (s *unsyncedStore) UpdateIfRevision(name, content string, meta *artifacts.Metadata, expected int) (*artifacts.Artifact, error) {
	s.updates++
	return &artifacts.Artifact{Name: name, Content: content, Metadata: *meta, Revision: expected + 1},
		fmt.Errorf("artifacts: %s: %w", name, artifacts.ErrPublishedUnsynced)
}

// A lesson that was published with its directory sync unconfirmed is stored.
// Save must hand back its name and revision beside the error, once, and not
// retry (a retry of Create would find it, and file a second revision).
func TestSaveReturnsAnUnsyncedCreateAsStored(t *testing.T) {
	st := &unsyncedStore{}
	name, rev, err := save(st, "gitpulse", "sess-1", parsed(t))
	if !errors.Is(err, artifacts.ErrPublishedUnsynced) {
		t.Fatalf("err = %v, want ErrPublishedUnsynced", err)
	}
	if name == "" || rev != 1 {
		t.Fatalf("a stored lesson came back as (%q, %d)", name, rev)
	}
	if st.creates != 1 || st.updates != 0 {
		t.Fatalf("writes = %d creates, %d updates; an unconfirmed sync must not be retried", st.creates, st.updates)
	}
}

func TestSaveReturnsAnUnsyncedUpdateAsStored(t *testing.T) {
	first, _, err := Save(mustStore(t), "gitpulse", "sess-1", parsed(t))
	if err != nil {
		t.Fatal(err)
	}
	st := &unsyncedStore{existing: &artifacts.Artifact{Name: first, Content: "x", Revision: 3}}
	name, rev, err := save(st, "gitpulse", "sess-2", parsed(t))
	if !errors.Is(err, artifacts.ErrPublishedUnsynced) {
		t.Fatalf("err = %v, want ErrPublishedUnsynced", err)
	}
	if name != first || rev != 4 {
		t.Fatalf("a stored revision came back as (%q, %d), want (%q, 4)", name, rev, first)
	}
	if st.updates != 1 || st.creates != 0 {
		t.Fatalf("writes = %d creates, %d updates; an unconfirmed sync must not be retried", st.creates, st.updates)
	}
}

func mustStore(t *testing.T) *artifacts.Store {
	t.Helper()
	s, _ := newStore(t)
	return s
}
