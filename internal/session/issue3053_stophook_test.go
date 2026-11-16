package session

import (
	"strings"
	"testing"
	"time"
)

// TestIssue3053_StopHookMessageHeading checks that the Stop-hook reason labels
// queued replies, progress updates and finished records as normal inbox
// delivery and keeps each record's own text.
func TestIssue3053_StopHookMessageHeading(t *testing.T) {
	reply := TransitionNotificationEvent{
		ChildSessionID: "sender", ChildTitle: "reviewer", TargetKind: InboxTargetKindReply,
		Tier: TurnTierUrgent, ToStatus: "waiting", Text: "Please review.\nThe patch is ready.",
	}
	progress := TransitionNotificationEvent{
		ChildSessionID: "worker", Tier: TurnTierInfo, ToStatus: "waiting", Text: "Still checking results.",
	}
	finished := TransitionNotificationEvent{
		ChildSessionID: "child", ChildTitle: "builder", Tier: TurnTierInfo,
		ToStatus: "waiting", Kind: transitionKindFinished, DoneStatus: "success", DoneSummary: "Build passed.",
	}
	replyText := "- [urgent] reply from=sender reviewer: waiting\n    Please review.\n    The patch is ready.\n"
	progressText := "- [info] worker (worker): waiting\n    Still checking results.\n"
	finishedText := "- [info] builder (child): success — Build passed.\n"
	for _, tc := range []struct {
		name   string
		events []TransitionNotificationEvent
		want   []string
	}{
		{"tagged reply", []TransitionNotificationEvent{reply}, []string{replyText}},
		{"waiting progress", []TransitionNotificationEvent{progress}, []string{progressText}},
		{"finished", []TransitionNotificationEvent{finished}, []string{finishedText}},
		{"mixed", []TransitionNotificationEvent{reply, progress, finished}, []string{replyText, progressText, finishedText}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatCompletionsForInjection(tc.events)
			if !strings.HasPrefix(got, "New session messages (normal inbox delivery; continue this turn to handle each):\n") {
				t.Errorf("missing neutral normal-delivery heading:\n%s", got)
			}
			if strings.Contains(got, "Child session(s) completed while you were busy") {
				t.Errorf("heading must not label every message a child completion:\n%s", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("missing record text %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestIssue3053_StopHookReplyDeliveredOnce checks that an urgent reply is
// delivered exactly once. Under the fork's sentinel-only Stop hook (fork issue
// #16) a reply does not block Stop: it stays queued for the inbox drain, which
// hands it over once under the normal-delivery heading.
func TestIssue3053_StopHookReplyDeliveredOnce(t *testing.T) {
	inboxTestHome(t)
	parent := "parent-stop-reply"
	if err := CommitToInbox(parent, TransitionNotificationEvent{
		ChildSessionID: "sender", ChildTitle: "reviewer", TargetKind: InboxTargetKindReply,
		Profile: "default", FromStatus: "running", ToStatus: "waiting",
		Tier: TurnTierUrgent, Text: "Please review the patch.",
		LastOutputHash: "reply-1", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if dec, blocked, err := DrainForStopHook(parent, false); err != nil || blocked || dec.Decision != "" {
		t.Fatalf("a reply must not block Stop: dec=%+v blocked=%v err=%v", dec, blocked, err)
	}
	got, err := DrainInboxForParent(parent)
	if err != nil || len(got) != 1 || got[0].ChildSessionID != "sender" {
		t.Fatalf("reply not delivered by the inbox drain: events=%+v err=%v", got, err)
	}
	text := FormatCompletionsForInjection(got)
	for _, want := range []string{"New session messages (normal inbox delivery;", "reply from=sender reviewer: waiting", "Please review the patch."} {
		if !strings.Contains(text, want) {
			t.Errorf("delivery missing %q:\n%s", want, text)
		}
	}
	if again, err := DrainInboxForParent(parent); err != nil || len(again) != 0 {
		t.Fatalf("second drain must not redeliver: events=%+v err=%v", again, err)
	}
}
