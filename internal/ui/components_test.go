package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/miwi/twitchpoint/internal/channels"
	"github.com/miwi/twitchpoint/internal/points"
)

// The streak column must print the label the points package produced, not
// a second copy of the wording. Checked through the real row renderer so
// the column widths are exercised too: a label wider than chColStreak
// would be silently truncated by padCell.
func TestRenderChannelRow_StreakColumn(t *testing.T) {
	cases := []struct {
		name   string
		streak points.StreakStatus
		want   string
	}{
		{
			name:   "hunting shows progress towards the target",
			streak: points.StreakStatus{Phase: points.StreakHunting, Watched: 8*time.Minute + 12*time.Second, Target: 12 * time.Minute},
			want:   "8:12/12:00",
		},
		{
			name:   "queued",
			streak: points.StreakStatus{Phase: points.StreakQueued, Target: 12 * time.Minute},
			want:   "queued",
		},
		{
			name:   "claimed",
			streak: points.StreakStatus{Phase: points.StreakClaimed, Target: 12 * time.Minute},
			want:   "claimed",
		},
		{
			name:   "watched past the target without a bonus",
			streak: points.StreakStatus{Phase: points.StreakNoBonus, Watched: 20 * time.Minute, Target: 12 * time.Minute},
			want:   "no bonus",
		},
		{
			name:   "idle prints a placeholder, never an empty cell",
			streak: points.StreakStatus{Phase: points.StreakIdle, Target: 12 * time.Minute},
			want:   "-",
		},
	}

	snap := channels.Snapshot{
		Login: "somechannel", DisplayName: "SomeChannel", ChannelID: "1",
		Priority: 2, IsOnline: true, IsWatching: true, GameName: "Escape from Tarkov",
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := renderChannelRow(snap, c.streak)
			if !strings.Contains(row, c.want) {
				t.Errorf("row does not contain %q:\n%s", c.want, row)
			}
		})
	}
}

// The header must have exactly as many columns as a row, or every column
// after the new one renders under the wrong heading.
func TestChannelTable_HeaderAndRowAgree(t *testing.T) {
	header := channelTableHeader()
	row := renderChannelRow(
		channels.Snapshot{DisplayName: "SomeChannel", ChannelID: "1", IsOnline: true},
		points.StreakStatus{Phase: points.StreakQueued, Target: 12 * time.Minute},
	)

	// Width is what actually has to line up; lipgloss pads every cell to a
	// fixed size, so comparing visible widths catches a missing cell that
	// counting separators would not.
	hw := visibleWidth(header)
	rw := visibleWidth(row)
	if hw != rw {
		t.Errorf("header width %d != row width %d — a column is missing on one side", hw, rw)
	}
	if !strings.Contains(header, "Streak") {
		t.Error("header has no Streak column")
	}
}

// visibleWidth strips ANSI escape sequences so the comparison counts
// printed characters, not styling bytes.
func visibleWidth(s string) int {
	r := []rune(strings.Split(s, "\n")[0])
	var n, i int
	for i < len(r) {
		if r[i] == 0x1b {
			for i < len(r) && r[i] != 'm' {
				i++
			}
			i++
			continue
		}
		n++
		i++
	}
	return n
}
