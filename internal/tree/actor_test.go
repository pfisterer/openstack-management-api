package tree

import "testing"

// The channel has to survive into the history, or the whole distinction is
// decoration. These pin the two ends of it: what an unset channel becomes, and
// that a named one arrives intact.

func TestActor_ChannelDefaultsToUI(t *testing.T) {
	if got := (Actor{Email: "a@b.c"}).Channel(); got != ChannelUI {
		t.Errorf("unset channel became %q, want %q", got, ChannelUI)
	}
	if got := UIActor("a@b.c").Channel(); got != ChannelUI {
		t.Errorf("UIActor reports %q, want %q", got, ChannelUI)
	}
	if got := (Actor{Email: "a@b.c", Via: ChannelMCP}).Channel(); got != ChannelMCP {
		t.Errorf("named channel became %q, want %q", got, ChannelMCP)
	}
}

// An entry written today must never carry an empty channel: empty already means
// "written before this field existed", and folding the two together would throw
// away the distinction on the day someone needs it.
func TestNewHistoryEntry_NeverLeavesTheChannelEmpty(t *testing.T) {
	for _, actor := range []Actor{
		{Email: "a@b.c"},
		UIActor("a@b.c"),
		{Email: "a@b.c", Via: ChannelMCP},
	} {
		entry := newHistoryEntry("created", actor, StatusPending)
		if entry.Via == "" {
			t.Errorf("actor %+v produced an entry with no channel", actor)
		}
		if entry.Actor != "a@b.c" {
			t.Errorf("actor recorded as %q, want the e-mail", entry.Actor)
		}
	}
}

func TestNewHistoryEntry_CarriesTheChannel(t *testing.T) {
	if got := newHistoryEntry("created", UIActor("a@b.c"), StatusPending).Via; got != ChannelUI {
		t.Errorf("UI change recorded as %q, want %q", got, ChannelUI)
	}
	if got := newHistoryEntry("created", Actor{Email: "a@b.c", Via: ChannelMCP}, StatusPending).Via; got != ChannelMCP {
		t.Errorf("agent change recorded as %q, want %q", got, ChannelMCP)
	}
}
