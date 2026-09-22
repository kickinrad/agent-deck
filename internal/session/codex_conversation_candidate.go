package session

import "strings"

// ValidCodexConversationCandidate requires native, resumable user-session
// metadata in this instance's Codex home and project. Read on every event:
// a missing or partially flushed rollout is unverified, not a cached rejection.
func (i *Instance) ValidCodexConversationCandidate(sessionID string) bool {
	if i == nil || !IsCodexCompatible(i.Tool) || strings.TrimSpace(i.EffectiveWorkingDir()) == "" {
		return false
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.ContainsAny(sessionID, `/\\*?[]`) {
		return false
	}
	meta := readCodexRolloutThreadMeta(codexRolloutPathInHome(sessionID, i.getCodexHomeDir()))
	return meta.ID == sessionID && meta.Cwd != "" &&
		normalizePath(meta.Cwd) == normalizePath(i.EffectiveWorkingDir()) &&
		meta.UserSession
}

// validCodexHook checks the payload (or legacy anchor) before any status,
// activity, acknowledgement, generation, or binding mutation.
func (i *Instance) validCodexHook(status *HookStatus) bool {
	if !IsCodexCompatible(i.Tool) {
		return true
	}
	sessionID := strings.TrimSpace(status.SessionID)
	if sessionID == "" {
		sessionID = ReadHookSessionAnchor(i.ID)
	}
	return i.ValidCodexConversationCandidate(sessionID)
}
