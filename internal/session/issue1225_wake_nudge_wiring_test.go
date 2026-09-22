package session

// Issue #1225 Tier-2 wiring — the wake-nudge was built + unit-tested
// (issue1225_wake_nudge_test.go) but NOTHING triggered it: an idle conductor
// only drained on its next heartbeat (up to ~14 min lag). These tests assert
// the producer commit chokepoint (commitEventToInbox, shared by the interactive
// running→waiting path AND the one-shot run-task completion path) fires a
// debounced, idle-only, best-effort wake-nudge to THAT parent the moment a
// completion durably lands — and that a dropped nudge is harmless because the
// durable record is still present for the next-turn drain.

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// newWakeNudgeFixture seeds a child→parent pair in a fresh profile and returns a
// notifier plus a finished event whose commit resolves to parentID. The parent
// is titled non-"conductor-" so resolveParentNotificationTarget keeps it live
// without a tmux UpdateStatus (the conductor-scoping policy is unit-tested
// separately in TestIssue1225_ParentIsNudgeableIdle).
func newWakeNudgeFixture(t *testing.T) (*TransitionNotifier, string, TransitionNotificationEvent) {
	t.Helper()
	inboxTestHome(t)
	profile := "_test-wake-nudge"
	storage, err := NewStorageWithProfile(profile)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { storage.Close() })

	now := time.Now()
	parentID := "wake-parent-1"
	child := &Instance{
		ID:              "wake-child-1",
		Title:           "worker",
		ProjectPath:     "/tmp/c",
		GroupPath:       DefaultGroupPath,
		ParentSessionID: parentID,
		Tool:            "claude",
		Status:          StatusRunning,
		CreatedAt:       now,
	}
	parent := &Instance{
		ID:          parentID,
		Title:       "atlas",
		IsConductor: true,
		ProjectPath: "/tmp/p",
		GroupPath:   DefaultGroupPath,
		Tool:        "claude",
		Status:      StatusIdle,
		CreatedAt:   now,
	}
	if err := storage.SaveWithGroups([]*Instance{child, parent}, nil); err != nil {
		t.Fatalf("save: %v", err)
	}

	event := TransitionNotificationEvent{
		ChildSessionID: child.ID,
		ChildTitle:     child.Title,
		Profile:        profile,
		DoneStatus:     "success",
		DoneSummary:    "done",
	}
	return NewTransitionNotifier(), parentID, event
}

// A completion that arrives while a Codex conductor is busy remains pending.
// The daemon observes idle later and submits exactly one wake; the inbox marker
// survives a fresh notifier so restart cannot submit the same turn again.
func TestIssue1225_BusyCodexCompletionReconcilesOnceAfterIdle(t *testing.T) {
	n, parentID, event := newWakeNudgeFixture(t)
	parent := &Instance{ID: parentID, Title: "atlas", IsConductor: true, Status: StatusRunning}
	sent := 0
	idle := false
	n.wake = &wakeNudgeWiring{
		nudger: NewWakeNudger(0),
		now:    func() time.Time { return time.Unix(4000, 0) },
		isIdle: func(*Instance) bool { return idle },
		send:   func(*Instance, string) error { sent++; return nil },
	}
	if result := n.NotifyFinished(event); result.DeliveryResult != transitionDeliveryCommitted {
		t.Fatalf("busy completion = %q, want %q", result.DeliveryResult, transitionDeliveryCommitted)
	}
	if sent != 0 {
		t.Fatalf("busy completion sent %d wakes, want 0", sent)
	}

	d := NewTransitionDaemon()
	d.notifier = n
	parent.Status = StatusIdle
	idle = true
	d.reconcilePendingInboxWakes(event.Profile, map[string]*Instance{parentID: parent}, map[string]string{parentID: string(StatusIdle)})
	if sent != 1 {
		t.Fatalf("idle reconciliation sent %d wakes, want 1", sent)
	}
	records := readInboxLines(t, parentID)
	if len(records) != 1 || records[0].WakeSubmission != wakeSubmissionUncertain {
		t.Fatalf("wake reservation not durable: %+v", records)
	}

	// A fresh notifier models a daemon restart. The persisted reservation blocks
	// a second send even though this process has an empty in-memory debounce map.
	restarted := NewTransitionNotifier()
	restarted.wake = &wakeNudgeWiring{
		nudger: NewWakeNudger(0),
		now:    func() time.Time { return time.Unix(4001, 0) },
		isIdle: func(*Instance) bool { return idle },
		send:   func(*Instance, string) error { sent++; return nil },
	}
	d.notifier = restarted
	d.reconcilePendingInboxWakes(event.Profile, map[string]*Instance{parentID: parent}, map[string]string{parentID: string(StatusIdle)})
	if sent != 1 {
		t.Fatalf("restart re-submitted completion: sent=%d, want 1", sent)
	}
}

// A successful commit fires exactly one wake-nudge, aimed at the resolved parent.
func TestIssue1225_CommitFiresWakeNudgeToParent(t *testing.T) {
	n, parentID, event := newWakeNudgeFixture(t)

	var mu sync.Mutex
	var sentTo []string
	n.wake = &wakeNudgeWiring{
		nudger: NewWakeNudger(0),
		now:    func() time.Time { return time.Unix(1000, 0) },
		isIdle: func(p *Instance) bool { return true },
		send: func(p *Instance, profile string) error {
			mu.Lock()
			sentTo = append(sentTo, p.ID)
			mu.Unlock()
			return nil
		},
	}

	res := n.NotifyFinished(event)
	if res.DeliveryResult != transitionDeliveryCommitted {
		t.Fatalf("commit result = %q, want %q", res.DeliveryResult, transitionDeliveryCommitted)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sentTo) != 1 || sentTo[0] != parentID {
		t.Fatalf("wake-nudge targets = %v, want exactly [%s]", sentTo, parentID)
	}
}

// A busy (non-idle) parent is never nudged — send-keys into a running pane only
// queues the keystroke (issue #36326). The commit still succeeds.
func TestIssue1225_CommitDoesNotNudgeBusyParent(t *testing.T) {
	n, _, event := newWakeNudgeFixture(t)
	sent := 0
	n.wake = &wakeNudgeWiring{
		nudger: NewWakeNudger(0),
		now:    func() time.Time { return time.Unix(1000, 0) },
		isIdle: func(p *Instance) bool { return false },
		send:   func(p *Instance, profile string) error { sent++; return nil },
	}
	res := n.NotifyFinished(event)
	if res.DeliveryResult != transitionDeliveryCommitted {
		t.Fatalf("commit must still succeed for a busy parent; got %q", res.DeliveryResult)
	}
	if sent != 0 {
		t.Fatalf("busy parent: send called %d times, want 0", sent)
	}
}

// Ordinary turn telemetry remains durable for an ordinary parent, but never
// wakes an idle parent. Only NotifyFinished carries the explicit sentinel that
// may interrupt.
func TestIssue1225_TransitionCommitsWithoutWake(t *testing.T) {
	n, parentID, event := newWakeNudgeFixture(t)
	sent := 0
	n.wake = &wakeNudgeWiring{
		nudger: NewWakeNudger(0),
		now:    func() time.Time { return time.Unix(1000, 0) },
		isIdle: func(*Instance) bool { return true },
		send:   func(*Instance, string) error { sent++; return nil },
	}
	event.FromStatus, event.ToStatus = string(StatusRunning), string(StatusWaiting)
	event.DoneStatus, event.DoneSummary = "", ""
	result := n.NotifyTransition(event)
	if result.DeliveryResult != transitionDeliveryCommitted {
		t.Fatalf("transition result = %q, want %q", result.DeliveryResult, transitionDeliveryCommitted)
	}
	if sent != 0 {
		t.Fatalf("routine transition woke parent %d times, want 0", sent)
	}
	if records := readInboxLines(t, parentID); len(records) != 1 || records[0].Kind == transitionKindFinished {
		t.Fatalf("routine telemetry was not retained as a transition: %+v", records)
	}
}

// Two completions landing in a burst collapse to ONE wake — the parent's drain
// consumes all pending records in the single woken turn, so the suppressed
// nudge loses nothing.
func TestIssue1225_RapidCommitsDebounceToOneNudge(t *testing.T) {
	n, _, event := newWakeNudgeFixture(t)
	sent := 0
	n.wake = &wakeNudgeWiring{
		nudger: NewWakeNudger(time.Minute),
		now:    func() time.Time { return time.Unix(2000, 0) },
		isIdle: func(p *Instance) bool { return true },
		send:   func(p *Instance, profile string) error { sent++; return nil },
	}
	n.NotifyFinished(event)
	n.NotifyFinished(event) // within the debounce window → suppressed
	if sent != 1 {
		t.Fatalf("rapid commits: send called %d times, want 1 (debounced)", sent)
	}
}

// A nudge that fails to send is harmless: the commit still reports committed AND
// the durable record is present for the next heartbeat/turn drain (wake ≠ deliver).
func TestIssue1225_NudgeSendErrorIsHarmless(t *testing.T) {
	n, parentID, event := newWakeNudgeFixture(t)
	n.wake = &wakeNudgeWiring{
		nudger: NewWakeNudger(0),
		now:    func() time.Time { return time.Unix(3000, 0) },
		isIdle: func(p *Instance) bool { return true },
		send:   func(p *Instance, profile string) error { return errors.New("pane gone") },
	}
	res := n.NotifyFinished(event)
	if res.DeliveryResult != transitionDeliveryCommitted {
		t.Fatalf("a failed nudge must not fail the commit; got %q", res.DeliveryResult)
	}
	got := readInboxLines(t, parentID)
	if len(got) != 1 {
		t.Fatalf("durable record count = %d, want 1 (drain-on-next-turn safety net)", len(got))
	}
}

// The PRODUCTION default wiring (not a test spy) is fully populated and routes
// its idle probe through the conductor-scoped gate end-to-end: a conductor-
// prefixed title is nudgeable only when idle, a busy conductor and a
// non-conductor leaf are not. This guards against a future refactor silently
// swapping defaultWakeNudgeWiring's isIdle for an unscoped probe.
func TestIssue1225_DefaultWiringUsesConductorIdleGate(t *testing.T) {
	w := defaultWakeNudgeWiring()
	if w == nil || w.nudger == nil || w.now == nil || w.isIdle == nil || w.send == nil {
		t.Fatalf("default wiring must populate every hook, got %+v", w)
	}
	if !w.isIdle(&Instance{ID: "c", Title: "renamed-atlas", IsConductor: true, Status: StatusIdle}) {
		t.Fatal("default wiring must nudge an idle conductor")
	}
	if w.isIdle(&Instance{ID: "c", Title: "conductor-x", Status: StatusRunning}) {
		t.Fatal("default wiring must NOT nudge a busy conductor (send-keys would only queue)")
	}
	if w.isIdle(&Instance{ID: "l", Title: "worker", Status: StatusIdle}) {
		t.Fatal("default wiring must NOT nudge a non-conductor leaf (no inbox drain → noise)")
	}
}

// The production idle-probe is conductor-scoped (only conductors drain an inbox)
// and only green when the pane is idle/waiting (not mid-turn).
func TestIssue1225_ParentIsNudgeableIdle(t *testing.T) {
	cases := []struct {
		title       string
		isConductor bool
		status      Status
		want        bool
	}{
		{"conductor-x", false, StatusIdle, true}, // legacy title fallback
		{"renamed-atlas", true, StatusWaiting, true},
		{"renamed-atlas", true, StatusRunning, false}, // busy: send-keys would only queue
		{"worker", false, StatusIdle, false},          // non-conductor leaf: no inbox drain → noise
		{"renamed-atlas", true, StatusError, false},
	}
	for _, c := range cases {
		p := &Instance{ID: "p", Title: c.title, IsConductor: c.isConductor, Status: c.status}
		if got := parentIsNudgeableIdle(p); got != c.want {
			t.Errorf("parentIsNudgeableIdle(title=%q,status=%q)=%v, want %v", c.title, c.status, got, c.want)
		}
	}
}
