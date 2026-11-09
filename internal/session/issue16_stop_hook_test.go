package session

import (
	"strings"
	"testing"
)

func TestIssue16_StopHookOnlyBlocksForAssertedOrTerminalChild(t *testing.T) {
	cases := []struct {
		name  string
		event TransitionNotificationEvent
		block bool
	}{
		{"plain legacy edge", TransitionNotificationEvent{}, false},
		{"plain info edge", TransitionNotificationEvent{Tier: TurnTierInfo}, false},
		{"question", TransitionNotificationEvent{Tier: TurnTierUrgent, Question: true, Text: "NEED: choose?"}, false},
		{"sentinel", TransitionNotificationEvent{Kind: transitionKindFinished, Tier: TurnTierUrgent, DoneStatus: "ok"}, true},
		{"error", TransitionNotificationEvent{ToStatus: "error", Tier: TurnTierUrgent}, true},
		{"stopped", TransitionNotificationEvent{ToStatus: "stopped"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inboxTestHome(t)
			parent := "issue16-parent"
			ev := tc.event
			ev.ChildSessionID = "issue16-child"
			ev.ChildTitle = "worker"
			ev.LastOutputHash = "turn:one"
			commitTestRecord(t, parent, ev)

			dec, blocked, err := DrainForStopHook(parent, false)
			if err != nil || blocked != tc.block {
				t.Fatalf("Stop hook: blocked=%v, want %v, decision=%+v, err=%v", blocked, tc.block, dec, err)
			}
			if tc.block {
				if !strings.HasPrefix(dec.Reason, "Child session(s) completed while you were busy") {
					t.Fatalf("completion header missing: %q", dec.Reason)
				}
				if _, again, err := DrainForStopHook(parent, true); err != nil || again {
					t.Fatalf("completion blocked twice: blocked=%v err=%v", again, err)
				}
			} else {
				got, err := DrainInboxForParent(parent)
				if err != nil || len(got) != 1 || got[0].ChildSessionID != ev.ChildSessionID {
					t.Fatalf("plain edge not retrievable by inbox drain: events=%+v err=%v", got, err)
				}
			}
		})
	}
}

func TestIssue16_PlainEdgeStaysInInboxWhenSentinelBlocks(t *testing.T) {
	inboxTestHome(t)
	parent := "issue16-mixed-parent"
	commitTestRecord(t, parent, TransitionNotificationEvent{ChildSessionID: "plain-child", ChildTitle: "plain", LastOutputHash: "turn:plain"})
	commitTestRecord(t, parent, TransitionNotificationEvent{ChildSessionID: "done-child", ChildTitle: "done", Kind: transitionKindFinished, DoneStatus: "ok", LastOutputHash: "turn:done"})

	dec, blocked, err := DrainForStopHook(parent, false)
	if err != nil || !blocked || !strings.Contains(dec.Reason, "done-child") || strings.Contains(dec.Reason, "plain-child") {
		t.Fatalf("Stop hook must show only completion: blocked=%v decision=%+v err=%v", blocked, dec, err)
	}
	got, err := DrainInboxForParent(parent)
	if err != nil || len(got) != 1 || got[0].ChildSessionID != "plain-child" {
		t.Fatalf("inbox drain must retain plain edge: events=%+v err=%v", got, err)
	}
}
