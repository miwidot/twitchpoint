package channels

import (
	"sync"
	"testing"
)

func TestRegistry_AddAndGet(t *testing.T) {
	r := New()
	s := NewState("alice", "Alice", "111")
	r.Add(s)

	got, ok := r.Get("111")
	if !ok {
		t.Fatal("Get(111) returned ok=false after Add")
	}
	if got != s {
		t.Errorf("Get(111) returned a different pointer: want %p, got %p", s, got)
	}
}

func TestRegistry_GetMiss(t *testing.T) {
	r := New()
	if _, ok := r.Get("missing"); ok {
		t.Error("Get(missing) returned ok=true on empty registry")
	}
}

func TestRegistry_GetByLogin(t *testing.T) {
	r := New()
	s := NewState("alice", "Alice", "111")
	r.Add(s)

	got, ok := r.GetByLogin("alice")
	if !ok {
		t.Fatal("GetByLogin(alice) returned ok=false")
	}
	if got.ChannelID != "111" {
		t.Errorf("GetByLogin(alice).ChannelID = %q, want 111", got.ChannelID)
	}
}

func TestRegistry_GetByLoginMiss(t *testing.T) {
	r := New()
	if _, ok := r.GetByLogin("nobody"); ok {
		t.Error("GetByLogin on empty registry returned ok=true")
	}
}

func TestRegistry_RemoveReturnsState(t *testing.T) {
	r := New()
	s := NewState("alice", "Alice", "111")
	r.Add(s)

	got, ok := r.Remove("111")
	if !ok {
		t.Fatal("Remove(111) returned ok=false")
	}
	if got != s {
		t.Errorf("Remove(111) returned different pointer")
	}
	if _, stillThere := r.Get("111"); stillThere {
		t.Error("Get(111) returned ok=true after Remove")
	}
	if _, stillThere := r.GetByLogin("alice"); stillThere {
		t.Error("GetByLogin(alice) returned ok=true after Remove — login index not cleared")
	}
}

func TestRegistry_RemoveMiss(t *testing.T) {
	r := New()
	if _, ok := r.Remove("missing"); ok {
		t.Error("Remove(missing) returned ok=true on empty registry")
	}
}

func TestRegistry_Len(t *testing.T) {
	r := New()
	if got := r.Len(); got != 0 {
		t.Errorf("Len() on empty = %d, want 0", got)
	}
	r.Add(NewState("a", "A", "1"))
	r.Add(NewState("b", "B", "2"))
	if got := r.Len(); got != 2 {
		t.Errorf("Len() after 2 adds = %d, want 2", got)
	}
}

func TestRegistry_StatesReturnsLivePointers(t *testing.T) {
	r := New()
	s := NewState("alice", "Alice", "111")
	r.Add(s)

	states := r.States()
	if len(states) != 1 {
		t.Fatalf("States() returned %d entries, want 1", len(states))
	}
	// Mutate via the returned pointer; verify Registry sees it.
	states[0].SetBalance(500)
	got, _ := r.Get("111")
	if got.Snapshot().PointsBalance != 500 {
		t.Error("State mutation via States() pointer not visible through Registry")
	}
}

func TestRegistry_Snapshots(t *testing.T) {
	r := New()
	r.Add(NewState("a", "A", "1"))
	r.Add(NewState("b", "B", "2"))

	snaps := r.Snapshots()
	if len(snaps) != 2 {
		t.Fatalf("Snapshots() returned %d, want 2", len(snaps))
	}
	// Snapshots are values, mutating the original State must not affect them.
	if s, _ := r.Get("1"); s != nil {
		s.SetBalance(999)
	}
	for _, snap := range snaps {
		if snap.PointsBalance == 999 {
			t.Error("Snapshot reflects post-snapshot mutation — should be a copy")
		}
	}
}

func TestRegistry_AddOverwritesByID(t *testing.T) {
	r := New()
	s1 := NewState("alice", "Alice", "111")
	r.Add(s1)
	s2 := NewState("alice", "Alice", "111") // same ID, fresh state
	r.Add(s2)

	got, _ := r.Get("111")
	if got != s2 {
		t.Error("Add with same ID did not overwrite — last writer should win")
	}
	if r.Len() != 1 {
		t.Errorf("Len after overwrite-add = %d, want 1", r.Len())
	}
}

func TestRegistry_ConcurrentAccess(t *testing.T) {
	r := New()
	const writers = 10
	const reads = 100
	var wg sync.WaitGroup

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			s := NewState(stringID("login", id), "Name", stringID("ch", id))
			r.Add(s)
		}(i)
	}
	for i := 0; i < reads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.States()
			_ = r.Snapshots()
			_ = r.Len()
			_, _ = r.Get("ch1")
			_, _ = r.GetByLogin("login1")
		}()
	}
	wg.Wait()
	if r.Len() != writers {
		t.Errorf("Len after concurrent adds = %d, want %d", r.Len(), writers)
	}
}

func stringID(prefix string, n int) string {
	const digits = "0123456789"
	if n == 0 {
		return prefix + "0"
	}
	var rev []byte
	for n > 0 {
		rev = append(rev, digits[n%10])
		n /= 10
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return prefix + string(rev)
}
