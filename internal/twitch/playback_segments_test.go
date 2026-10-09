package twitch

import "testing"

const samplePlaylist = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:2
#EXTINF:2.000,
https://video-edge-a.example.net/v1/segment/aaa-1.ts
#EXTINF:2.000,
https://video-edge-a.example.net/v1/segment/aaa-2.ts
#EXTINF:2.000,
https://video-edge-a.example.net/v1/segment/aaa-3.ts
`

// The whole point of the fix: every segment in the playlist is returned, in
// order. Taking only the last one consumed ~1 segment per poll while the
// stream published several, which is what froze the drop counter.
func TestMediaSegments_ReturnsAllInOrder(t *testing.T) {
	got := mediaSegments(samplePlaylist)
	if len(got) != 3 {
		t.Fatalf("got %d segments, want 3: %v", len(got), got)
	}
	if got[0] != "https://video-edge-a.example.net/v1/segment/aaa-1.ts" {
		t.Fatalf("first segment wrong: %s", got[0])
	}
	if got[2] != "https://video-edge-a.example.net/v1/segment/aaa-3.ts" {
		t.Fatalf("last segment must be newest, got: %s", got[2])
	}
}

// Playlist URLs and directives must never be mistaken for media segments.
func TestMediaSegments_IgnoresNonSegments(t *testing.T) {
	playlist := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=1
https://video-weaver.example.net/v1/playlist/abc.m3u8
#EXTINF:2.000,
https://video-edge-a.example.net/v1/segment/only-1.ts
`
	got := mediaSegments(playlist)
	if len(got) != 1 || got[0] != "https://video-edge-a.example.net/v1/segment/only-1.ts" {
		t.Fatalf("expected only the media segment, got %v", got)
	}
}

func TestMediaSegments_EmptyPlaylist(t *testing.T) {
	if got := mediaSegments("#EXTM3U\n"); len(got) != 0 {
		t.Fatalf("expected no segments, got %v", got)
	}
}

func newProberWithChannel(login string) *StreamProber {
	p := &StreamProber{
		channels: map[string]*proberChannel{
			login: {login: login, seenSegments: make(map[string]struct{})},
		},
	}
	return p
}

// Re-requesting a segment earns nothing, so a segment is handed out once.
func TestUnseenSegments_DeduplicatesAcrossPolls(t *testing.T) {
	p := newProberWithChannel("alice")
	segs := []string{"a", "b", "c"}

	first := p.unseenSegments("alice", segs)
	if len(first) != 3 {
		t.Fatalf("first poll should yield all 3, got %v", first)
	}

	// Same playlist again — nothing new to consume.
	if again := p.unseenSegments("alice", segs); len(again) != 0 {
		t.Fatalf("repeated playlist must yield nothing, got %v", again)
	}

	// Playlist advances by one.
	next := p.unseenSegments("alice", []string{"b", "c", "d"})
	if len(next) != 1 || next[0] != "d" {
		t.Fatalf("expected only the new segment d, got %v", next)
	}
}

// The caller treats the final entry as newest for its byte-capped GET.
func TestUnseenSegments_PreservesOrder(t *testing.T) {
	p := newProberWithChannel("alice")
	got := p.unseenSegments("alice", []string{"s1", "s2", "s3"})
	if len(got) != 3 || got[0] != "s1" || got[2] != "s3" {
		t.Fatalf("order not preserved: %v", got)
	}
}

// A multi-hour session must not grow the dedup cache without bound.
func TestUnseenSegments_CacheIsBounded(t *testing.T) {
	p := newProberWithChannel("alice")
	for i := 0; i < maxSeenSegments*2; i++ {
		p.unseenSegments("alice", []string{string(rune('a'+i%26)) + "-" + itoa(i)})
	}
	ch := p.channels["alice"]
	if len(ch.seenSegments) > maxSeenSegments {
		t.Fatalf("cache grew to %d, bound is %d", len(ch.seenSegments), maxSeenSegments)
	}
	if len(ch.seenOrder) > maxSeenSegments {
		t.Fatalf("order list grew to %d, bound is %d", len(ch.seenOrder), maxSeenSegments)
	}
}

// A channel stopped mid-fetch must not panic or resurrect state.
func TestUnseenSegments_UnknownChannelYieldsNothing(t *testing.T) {
	p := &StreamProber{channels: map[string]*proberChannel{}}
	if got := p.unseenSegments("ghost", []string{"a"}); got != nil {
		t.Fatalf("stopped channel must yield nothing, got %v", got)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
