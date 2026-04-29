package channels

import "sync"

// Registry is the process-wide channel state store. It owns the
// channelID→State map and the login→channelID lookup index, and serializes
// access via a single RWMutex. Per-channel mutations still go through the
// State's own mutex (returned by the lookup methods).
//
// Last-writer-wins on Add(): re-adding a State with the same ChannelID
// overwrites the previous entry, matching the existing `m[k] = v` semantics
// the registry replaces.
type Registry struct {
	mu      sync.RWMutex
	byID    map[string]*State
	byLogin map[string]string // login -> channelID
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{
		byID:    make(map[string]*State),
		byLogin: make(map[string]string),
	}
}

// Add inserts (or replaces) a state under its ChannelID and indexes it by Login.
func (r *Registry) Add(s *State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[s.ChannelID] = s
	r.byLogin[s.Login] = s.ChannelID
}

// Get looks up a state by channel ID.
func (r *Registry) Get(channelID string) (*State, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.byID[channelID]
	return s, ok
}

// GetByLogin looks up a state by login (case-sensitive — callers must
// lowercase first; the existing farmer code already does).
func (r *Registry) GetByLogin(login string) (*State, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byLogin[login]
	if !ok {
		return nil, false
	}
	s, ok := r.byID[id]
	return s, ok
}

// Remove deletes the channel from both indexes and returns the removed
// state so callers can perform cleanup (Spade.StopWatching, prober.Stop, …).
// Returns ok=false if the channel is not registered.
func (r *Registry) Remove(channelID string) (*State, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byID[channelID]
	if !ok {
		return nil, false
	}
	delete(r.byID, channelID)
	delete(r.byLogin, s.Login)
	return s, true
}

// Len returns the number of tracked channels.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byID)
}

// States returns a slice copy of every State pointer. The slice is a
// snapshot of the registry at call time; the State pointers themselves
// remain live and mutable. The lock is only held while the slice is built.
func (r *Registry) States() []*State {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*State, 0, len(r.byID))
	for _, s := range r.byID {
		out = append(out, s)
	}
	return out
}

// Snapshots returns an immutable Snapshot of every channel. Each Snapshot
// is independent of subsequent State mutations.
func (r *Registry) Snapshots() []Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Snapshot, 0, len(r.byID))
	for _, s := range r.byID {
		out = append(out, s.Snapshot())
	}
	return out
}
