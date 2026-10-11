package tree

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// The channel has to survive into the history, or the whole distinction is
// decoration. These pin the two ends of it: what an unset channel becomes, and
// that a named one arrives intact.

func TestActor_ChannelDefaultsToUI(t *testing.T) {
	if got := (model.Actor{Email: "a@b.c"}).Channel(); got != model.ChannelUI {
		t.Errorf("unset channel became %q, want %q", got, model.ChannelUI)
	}
	if got := model.UIActor("a@b.c").Channel(); got != model.ChannelUI {
		t.Errorf("UIActor reports %q, want %q", got, model.ChannelUI)
	}
	if got := (model.Actor{Email: "a@b.c", Via: model.ChannelMCP}).Channel(); got != model.ChannelMCP {
		t.Errorf("named channel became %q, want %q", got, model.ChannelMCP)
	}
}

// An entry written today must never carry an empty channel: empty already means
// "written before this field existed", and folding the two together would throw
// away the distinction on the day someone needs it.
func TestNewHistoryEntry_NeverLeavesTheChannelEmpty(t *testing.T) {
	for _, actor := range []model.Actor{
		{Email: "a@b.c"},
		model.UIActor("a@b.c"),
		{Email: "a@b.c", Via: model.ChannelMCP},
	} {
		entry := model.NewHistoryEntry("created", actor, model.StatusPending)
		if entry.Via == "" {
			t.Errorf("actor %+v produced an entry with no channel", actor)
		}
		if entry.Actor != "a@b.c" {
			t.Errorf("actor recorded as %q, want the e-mail", entry.Actor)
		}
	}
}

func TestNewHistoryEntry_CarriesTheChannel(t *testing.T) {
	if got := model.NewHistoryEntry("created", model.UIActor("a@b.c"), model.StatusPending).Via; got != model.ChannelUI {
		t.Errorf("UI change recorded as %q, want %q", got, model.ChannelUI)
	}
	if got := model.NewHistoryEntry("created", model.Actor{Email: "a@b.c", Via: model.ChannelMCP}, model.StatusPending).Via; got != model.ChannelMCP {
		t.Errorf("agent change recorded as %q, want %q", got, model.ChannelMCP)
	}
}
