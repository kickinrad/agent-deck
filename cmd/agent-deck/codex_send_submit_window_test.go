package main

import "testing"

// A Codex turn becomes visible to pane-based status detection a few seconds
// after the body lands in the composer (measured ~4s live). Arrival checks
// stopped after arrivalVerifyChecks, so a delivered message that the agent
// took up was reported as typed-not-submitted. Once the body has arrived,
// verification must keep watching for the turn within the send budget.
func TestCodexSend_SlowTurnStartAfterArrival_IsSubmitted(t *testing.T) {
	const msg = "Do not use tools. Reply with exactly PROBE-4471."
	statuses := []string{"waiting"} // pre-send baseline
	for i := 0; i < arrivalVerifyChecks+3; i++ {
		statuses = append(statuses, "waiting")
	}
	statuses = append(statuses, "active")

	mock := &mockSendRetryTarget{
		statuses: statuses,
		panes:    []string{"› \n", "› " + msg + "\n"},
	}

	delivery, err := sendWithRetryTarget(mock, msg, true, sendRetryOptions{
		maxRetries: 50, checkDelay: 0, tool: "codex",
	})
	if err != nil || delivery != deliverySubmitted {
		t.Fatalf("want submitted, got %q (err=%v)", delivery, err)
	}
}

// The extended watch is bounded by the send budget: a body that arrives and
// is never taken up still fails as typed-not-submitted.
func TestCodexSend_ArrivedButNeverStarted_StillTypedWithinBudget(t *testing.T) {
	const msg = "Do not use tools. Reply with exactly PROBE-4471."
	mock := &mockSendRetryTarget{
		statuses: []string{"waiting"},
		panes:    []string{"› \n", "› " + msg + "\n"},
	}

	delivery, err := sendWithRetryTarget(mock, msg, true, sendRetryOptions{
		maxRetries: 50, checkDelay: 0, tool: "codex",
	})
	if err == nil || delivery != deliveryTyped {
		t.Fatalf("want typed failure, got %q (err=%v)", delivery, err)
	}
}

// A Codex turn can start and finish between status polls, so pane-based
// status never shows it active (observed live on .6). The notify hook's turn
// marker changing across the send is the submission evidence.
func TestCodexSend_HookTurnMarkerChange_IsSubmitted(t *testing.T) {
	const msg = "Do not use tools. Reply with exactly PROBE-6003."
	calls := 0
	mock := &mockSendRetryTarget{
		statuses: []string{"waiting"},
		panes:    []string{"› \n", "› " + msg + "\n"},
	}

	delivery, err := sendWithRetryTarget(mock, msg, true, sendRetryOptions{
		maxRetries: 50, checkDelay: 0, tool: "codex",
		turnMarker: func() string {
			calls++
			if calls <= 3 {
				return "turn-a|turn-a"
			}
			return "turn-b|turn-b"
		},
	})
	if err != nil || delivery != deliverySubmitted {
		t.Fatalf("want submitted, got %q (err=%v)", delivery, err)
	}
}

// A marker change while the agent was already busy is not attributable to
// this send.
func TestCodexSend_HookTurnMarkerChangeWhileBusy_IsNotSubmission(t *testing.T) {
	const msg = "Do not use tools. Reply with exactly PROBE-6003."
	calls := 0
	mock := &mockSendRetryTarget{
		statuses: []string{"active"},
		panes:    []string{"› \n", "› " + msg + "\n"},
	}

	delivery, _ := sendWithRetryTarget(mock, msg, true, sendRetryOptions{
		maxRetries: 20, checkDelay: 0, tool: "codex",
		turnMarker: func() string {
			calls++
			if calls == 1 {
				return "turn-a|turn-a"
			}
			return "turn-b|turn-b"
		},
	})
	if delivery == deliverySubmitted {
		t.Fatal("a turn change on an already-busy agent must not certify this send")
	}
}
