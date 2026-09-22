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
// has an ordinary title: an explicit parent link, not conductor residency,
// governs delivery (the gate is unit-tested separately in
// TestIssue1225_ParentIsNudgeableIdle).
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
		Title:       "orchestrator",
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

// A successful commit fires exactly one wake-nudge, aimed at the resolved parent.
func TestIssue1225_CommitFiresWakeNudgeToParent(t *testing.T) {
	n, parentID, event := newWakeNudgeFixture(t)

	var mu sync.Mutex
	var sentTo []string
	n.wake = &wakeNudgeWiring{
		nudger: NewWakeNudger(0),
		now:    func() time.Time { return time.Unix(1000, 0) },
		isIdle: func(p *Instance, _ string) bool { return true },
		send: func(p *Instance, profile, _ string) error {
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
		isIdle: func(p *Instance, _ string) bool { return false },
		send:   func(p *Instance, profile, _ string) error { sent++; return nil },
	}
	res := n.NotifyFinished(event)
	if res.DeliveryResult != transitionDeliveryCommitted {
		t.Fatalf("commit must still succeed for a busy parent; got %q", res.DeliveryResult)
	}
	if sent != 0 {
		t.Fatalf("busy parent: send called %d times, want 0", sent)
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
		isIdle: func(p *Instance, _ string) bool { return true },
		send:   func(p *Instance, profile, _ string) error { sent++; return nil },
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
		isIdle: func(p *Instance, _ string) bool { return true },
		send:   func(p *Instance, profile, _ string) error { return errors.New("pane gone") },
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
// its idle probe through the parent gate end-to-end: any explicit parent that
// runs an agent is nudgeable only when idle; a busy parent and a shell are
// not. This guards against a future refactor silently swapping
// defaultWakeNudgeWiring's isIdle for an unscoped probe.
func TestIssue1225_DefaultWiringUsesParentIdleGate(t *testing.T) {
	withNoopStatusProbe(t)
	w := defaultWakeNudgeWiring()
	if w == nil || w.nudger == nil || w.now == nil || w.isIdle == nil || w.send == nil {
		t.Fatalf("default wiring must populate every hook, got %+v", w)
	}
	if !w.isIdle(&Instance{ID: "c", Title: "conductor-x", Status: StatusIdle}, "parent") {
		t.Fatal("default wiring must nudge an idle conductor")
	}
	if w.isIdle(&Instance{ID: "c", Title: "conductor-x", Status: StatusRunning}, "parent") {
		t.Fatal("default wiring must NOT nudge a busy conductor (send-keys would only queue)")
	}
	if !w.isIdle(&Instance{ID: "l", Title: "worker", Tool: "claude", Status: StatusIdle}, "parent") {
		t.Fatal("default wiring must nudge an idle ordinary parent")
	}
	if w.isIdle(&Instance{ID: "s", Title: "worker", Tool: "shell", Status: StatusIdle}, "parent") {
		t.Fatal("default wiring must NOT type a wake line into a shell")
	}
}

// The production idle-probe accepts any explicit parent that runs an agent,
// and is only green when the pane is idle/waiting (not mid-turn).
func TestIssue1225_ParentIsNudgeableIdle(t *testing.T) {
	withNoopStatusProbe(t)
	cases := []struct {
		title  string
		tool   string
		status Status
		want   bool
	}{
		{"conductor-x", "claude", StatusIdle, true},
		{"conductor-x", "claude", StatusWaiting, true},
		{"conductor-x", "claude", StatusRunning, false}, // busy: send-keys would only queue
		{"worker", "claude", StatusIdle, true},          // explicit parent, whatever its title
		{"worker", "codex", StatusWaiting, true},
		{"worker", "shell", StatusIdle, false}, // a shell would execute the line
		{"conductor-x", "claude", StatusError, false},
	}
	for _, c := range cases {
		p := &Instance{ID: "p", Title: c.title, Tool: c.tool, Status: c.status}
		if got := parentIsNudgeableIdle(p, "parent"); got != c.want {
			t.Errorf("parentIsNudgeableIdle(title=%q,status=%q)=%v, want %v", c.title, c.status, got, c.want)
		}
	}
}

// wakeSpy is a wiring whose idle probe and sends are counted. debounce and
// clock drive the real WakeNudger; a zero clock means the wall clock.
type wakeSpy struct {
	idle     bool
	probes   int
	sent     int
	debounce time.Duration
	clock    time.Time
}

func (s *wakeSpy) wiring() *wakeNudgeWiring {
	return &wakeNudgeWiring{
		nudger: NewWakeNudger(s.debounce),
		now: func() time.Time {
			if s.clock.IsZero() {
				return time.Now()
			}
			return s.clock
		},
		isIdle: func(*Instance, string) bool { s.probes++; return s.idle },
		send:   func(*Instance, string, string) error { s.sent++; return nil },
	}
}

// reconcileIdle runs the daemon's idle reconciliation for parentID through n.
func reconcileIdle(n *TransitionNotifier, profile, parentID string) {
	reconcileIdleTitled(n, profile, parentID, "orchestrator")
}

func reconcileIdleTitled(n *TransitionNotifier, profile, parentID, title string) {
	d := NewTransitionDaemon()
	d.notifier = n
	parent := &Instance{ID: parentID, Title: title, Tool: "claude", Status: StatusIdle}
	child := &Instance{ID: "wake-child-1", ParentSessionID: parentID, Status: StatusWaiting}
	d.reconcilePendingInboxWakes(profile, map[string]*Instance{parentID: parent, child.ID: child},
		map[string]string{parentID: string(StatusIdle), child.ID: string(StatusWaiting)})
}

// unmarkedRecords counts pending records that hold no wake submission.
func unmarkedRecords(t *testing.T, parentID string) int {
	t.Helper()
	n := 0
	for _, ev := range readInboxLines(t, parentID) {
		if ev.WakeSubmission == "" {
			n++
		}
	}
	return n
}

// A nudge the debounce rejects reserves nothing: the second of two urgent
// turns 100ms apart stays retryable and the next pass wakes once for it.
func TestDurableSessions_DebouncedWakeLeavesRecordRetryable(t *testing.T) {
	n, parentID, event := newWakeNudgeFixture(t)
	spy := &wakeSpy{idle: true, debounce: 500 * time.Millisecond, clock: time.Unix(5000, 0)}
	n.wake = spy.wiring()
	first, second := event, event
	first.DoneSummary, second.DoneSummary = "first result", "second result"
	n.NotifyFinished(first)
	spy.clock = spy.clock.Add(100 * time.Millisecond)
	n.NotifyFinished(second)
	if spy.sent != 1 || unmarkedRecords(t, parentID) != 1 {
		t.Fatalf("debounced turn: sent=%d unmarked=%d, want 1 and 1", spy.sent, unmarkedRecords(t, parentID))
	}

	spy.clock = spy.clock.Add(10 * time.Second)
	reconcileIdle(n, event.Profile, parentID)
	reconcileIdle(n, event.Profile, parentID)
	if spy.sent != 2 || unmarkedRecords(t, parentID) != 0 {
		t.Fatalf("retry of the debounced turn: sent=%d unmarked=%d, want 2 and 0", spy.sent, unmarkedRecords(t, parentID))
	}
}

// A digest nudge just before reconciliation in the same pass debounces the
// retry; the record stays unmarked and the next pass wakes once for it.
func TestDurableSessions_DigestNudgeDoesNotSwallowRetry(t *testing.T) {
	n, parentID, event := newWakeNudgeFixture(t)
	spy := &wakeSpy{debounce: 500 * time.Millisecond, clock: time.Unix(6000, 0)}
	n.wake = spy.wiring()
	n.NotifyFinished(event) // busy: committed, not woken
	spy.idle = true
	parent := &Instance{ID: parentID, Title: "orchestrator", Tool: "claude", Status: StatusIdle}
	if !n.fireDigestNudge(parent, event.Profile, "digest") {
		t.Fatal("precondition: the digest nudge must send")
	}
	reconcileIdle(n, event.Profile, parentID)
	if spy.sent != 1 || unmarkedRecords(t, parentID) != 1 {
		t.Fatalf("same-pass retry: sent=%d unmarked=%d, want 1 and 1", spy.sent, unmarkedRecords(t, parentID))
	}
	spy.clock = spy.clock.Add(10 * time.Second)
	reconcileIdle(n, event.Profile, parentID)
	if spy.sent != 2 || unmarkedRecords(t, parentID) != 0 {
		t.Fatalf("next-pass retry: sent=%d unmarked=%d, want 2 and 0", spy.sent, unmarkedRecords(t, parentID))
	}
}

// A conductor's pending urgent record is left to upstream's heartbeat and
// outbox path: the retry never probes or wakes a conductor.
func TestDurableSessions_RetryLeavesConductorsToUpstream(t *testing.T) {
	n, parentID, event := newWakeNudgeFixture(t)
	spy := &wakeSpy{}
	n.wake = spy.wiring()
	n.NotifyFinished(event)
	spy.idle, spy.probes = true, 0
	reconcileIdleTitled(n, event.Profile, parentID, "conductor-ops")
	if spy.sent != 0 || spy.probes != 0 || unmarkedRecords(t, parentID) != 1 {
		t.Fatalf("conductor retried: sent=%d probes=%d unmarked=%d", spy.sent, spy.probes, unmarkedRecords(t, parentID))
	}
}

// Past the per-child bound, an urgent turn folded into a woken overflow
// digest reopens it for one wake; an info fold keeps the submission.
func TestDurableSessions_UrgentFoldReopensDigestWake(t *testing.T) {
	inboxTestHome(t)
	parentID, childID := "fold-parent", "fold-child"
	fillPendingTurns(t, parentID, childID, maxPendingTurnsPerChild)
	fold := func(hash, tier string) TransitionNotificationEvent {
		stored, outcome, err := commitToInbox(parentID, TransitionNotificationEvent{
			ChildSessionID: childID, FromStatus: "running", ToStatus: "waiting",
			LastOutputHash: hash, Tier: tier, Timestamp: time.Now(),
		})
		if err != nil || outcome != inboxCommitDigested {
			t.Fatalf("fold %s: outcome=%v err=%v", hash, outcome, err)
		}
		return stored
	}
	fold("urgent-1", TurnTierUrgent)
	if ok, err := reserveInboxWake(parentID, func(ev TransitionNotificationEvent) bool { return ev.OverflowTurns > 0 }); err != nil || !ok {
		t.Fatalf("reserve digest: %v %v", ok, err)
	}
	if got := fold("info-1", TurnTierInfo); got.WakeSubmission != wakeSubmissionUncertain {
		t.Fatalf("an info fold dropped the digest's submission: %+v", got)
	}
	if got := fold("urgent-2", TurnTierUrgent); got.WakeSubmission != "" {
		t.Fatalf("an urgent fold kept the digest's submission, so it can never wake: %+v", got)
	}
}

// A completion committed while its parent was busy is retried once the daemon
// sees the parent idle, exactly once: the reservation is durable, so neither a
// later pass nor a restarted daemon (empty debounce map) sends it again.
func TestDurableSessions_BusyParentCompletionWakesOnceWhenIdle(t *testing.T) {
	n, parentID, event := newWakeNudgeFixture(t)
	spy := &wakeSpy{}
	n.wake = spy.wiring()
	if res := n.NotifyFinished(event); res.DeliveryResult != transitionDeliveryCommitted {
		t.Fatalf("busy completion = %q, want committed", res.DeliveryResult)
	}
	if spy.sent != 0 {
		t.Fatalf("busy parent woken %d times, want 0", spy.sent)
	}

	spy.idle = true
	reconcileIdle(n, event.Profile, parentID)
	if spy.sent != 1 {
		t.Fatalf("idle reconciliation sent %d wakes, want 1", spy.sent)
	}
	if got := readInboxLines(t, parentID); len(got) != 1 || got[0].WakeSubmission != wakeSubmissionUncertain {
		t.Fatalf("wake reservation not durable: %+v", got)
	}

	reconcileIdle(n, event.Profile, parentID)
	restarted := NewTransitionNotifier()
	restarted.wake = spy.wiring()
	reconcileIdle(restarted, event.Profile, parentID)
	if spy.sent != 1 {
		t.Fatalf("reconciliation resubmitted the completion: sent=%d, want 1", spy.sent)
	}
	// The wake never consumes: the record still drains on the parent's turn.
	if got, err := DrainInboxForParent(parentID); err != nil || len(got) != 1 {
		t.Fatalf("drain after wake: delivered=%d err=%v", len(got), err)
	}
}

// One finished child turn nudges an idle parent at most once: the commit-time
// wake reserves the record, so the same daemon pass's reconciliation, a
// re-emit of the turn by a restarted notifier, and the next pass all stay
// quiet. A reserved record never reaches the bounded status probe.
func TestDurableSessions_FinishedTurnNudgesParentAtMostOnce(t *testing.T) {
	n, parentID, event := newWakeNudgeFixture(t)
	spy := &wakeSpy{idle: true}
	n.wake = spy.wiring()
	n.NotifyFinished(event)
	if spy.sent != 1 {
		t.Fatalf("idle parent woken %d times on commit, want 1", spy.sent)
	}

	probes := spy.probes
	reconcileIdle(n, event.Profile, parentID)
	if spy.probes != probes {
		t.Fatalf("a reserved record reached the status probe (%d -> %d)", probes, spy.probes)
	}
	restarted := NewTransitionNotifier()
	restarted.wake = spy.wiring()
	restarted.NotifyFinished(event)
	reconcileIdle(restarted, event.Profile, parentID)
	if spy.sent != 1 {
		t.Fatalf("one finished turn woke the parent %d times, want 1", spy.sent)
	}
}

// An info record never buys a wake of its own, not even by reconciliation:
// it rides the parent's next turn or the info digest (issue #2469).
func TestDurableSessions_ReconcileLeavesInfoForTheDigest(t *testing.T) {
	n, parentID, event := newWakeNudgeFixture(t)
	spy := &wakeSpy{}
	n.wake = spy.wiring()
	event.FromStatus, event.ToStatus = string(StatusRunning), string(StatusWaiting)
	event.DoneStatus, event.DoneSummary = "", ""
	event.Tier, event.Text = TurnTierInfo, "routine progress"
	if res := n.NotifyTransition(event); res.DeliveryResult != transitionDeliveryCommitted {
		t.Fatalf("info turn = %q, want committed", res.DeliveryResult)
	}
	spy.idle = true
	reconcileIdle(n, event.Profile, parentID)
	if spy.sent != 0 || spy.probes != 0 {
		t.Fatalf("info record woke the parent: sent=%d probes=%d", spy.sent, spy.probes)
	}
	if got := readInboxLines(t, parentID); len(got) != 1 || got[0].Tier != TurnTierInfo {
		t.Fatalf("info record not retained: %+v", got)
	}
}

// withNoopStatusProbe swaps the status-probe seam for one that leaves the
// instance's persisted status untouched, so a table test can pin the gate's
// decision per status without a tmux server.
func withNoopStatusProbe(t *testing.T) {
	t.Helper()
	orig := updateInstanceStatus.Load().(statusProbeFunc)
	updateInstanceStatus.Store(statusProbeFunc(func(*Instance) error { return nil }))
	t.Cleanup(func() { updateInstanceStatus.Store(orig) })
}

// Review round 2 (P2-D): the idle gate must consult a FRESH status, not the
// registry row as last persisted. A parent whose row still says running but
// whose hook/pane state is idle is woken; a probe that overruns the daemon's
// budget is treated as not idle and the nudge is skipped.
func TestWakeNudge_IdleGateUsesFreshStatus(t *testing.T) {
	orig := updateInstanceStatus.Load().(statusProbeFunc)
	origBudget := statusProbeBudget
	t.Cleanup(func() {
		updateInstanceStatus.Store(orig)
		statusProbeBudget = origBudget
	})

	// Stale running row, fresh probe says idle: nudgeable.
	updateInstanceStatus.Store(statusProbeFunc(func(inst *Instance) error {
		inst.Status = StatusIdle
		return nil
	}))
	stale := &Instance{ID: "p", Title: "conductor-x", Status: StatusRunning}
	if !parentIsNudgeableIdle(stale, "parent") {
		t.Fatal("a stale running row must not withhold the wake when the fresh probe says idle")
	}

	// Stale idle row, fresh probe says running (mid-turn): not nudgeable.
	updateInstanceStatus.Store(statusProbeFunc(func(inst *Instance) error {
		inst.Status = StatusRunning
		return nil
	}))
	busy := &Instance{ID: "p", Title: "conductor-x", Status: StatusIdle}
	if parentIsNudgeableIdle(busy, "parent") {
		t.Fatal("a stale idle row must not send a nudge into a pane the fresh probe reports mid-turn")
	}

	// Probe overruns the budget: bounded, and not idle.
	statusProbeBudget = 50 * time.Millisecond
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	updateInstanceStatus.Store(statusProbeFunc(func(*Instance) error {
		<-block
		return nil
	}))
	hung := &Instance{ID: "p", Title: "conductor-x", Status: StatusIdle}
	start := time.Now()
	if parentIsNudgeableIdle(hung, "parent") {
		t.Fatal("a probe that overruns the budget must not report idle")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("gate must return at the probe budget, took %v", elapsed)
	}
}
