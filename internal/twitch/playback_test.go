package twitch

import "testing"

const sampleMediaPlaylist = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:2
#EXT-X-MAP:URI="https://video-edge.example/v1/segment/init.mp4?sig=x"
#EXTINF:2.000,live
https://video-edge.example/v1/segment/CgAB-1.mp4?sig=a
#EXTINF:2.000,live
https://video-edge.example/v1/segment/CgAB-2.mp4?sig=b
#EXT-X-TWITCH-PREFETCH:https://video-edge.example/v1/segment/CgAB-3.mp4?sig=c
https://video-edge.example/v1/playlist/next.m3u8
`

func TestParseSegmentURLs(t *testing.T) {
	got := parseSegmentURLs(sampleMediaPlaylist)
	want := []string{
		"https://video-edge.example/v1/segment/CgAB-1.mp4?sig=a",
		"https://video-edge.example/v1/segment/CgAB-2.mp4?sig=b",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d segments, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("segment %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSegmentKeyStripsQuery(t *testing.T) {
	if k := segmentKey("https://h/segment/a.mp4?sig=1"); k != "https://h/segment/a.mp4" {
		t.Errorf("got %q", k)
	}
	if k := segmentKey("https://h/segment/a.mp4"); k != "https://h/segment/a.mp4" {
		t.Errorf("got %q", k)
	}
}

func TestMarkSeenIsBounded(t *testing.T) {
	p := NewStreamProber(nil, "", "", "", nil)
	ch := &proberChannel{login: "x", seen: make(map[string]struct{})}
	for i := 0; i < seenSegmentsCap+10; i++ {
		p.markSeen(ch, string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	if len(ch.seen) != seenSegmentsCap || len(ch.seenOrder) != seenSegmentsCap {
		t.Fatalf("cache not bounded: map=%d order=%d", len(ch.seen), len(ch.seenOrder))
	}
	if _, ok := ch.seen["a0"]; ok {
		t.Errorf("oldest entry should have been evicted")
	}
}
