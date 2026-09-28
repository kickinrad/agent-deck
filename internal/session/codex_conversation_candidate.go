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
	meta := readCodexRolloutThreadMeta(codexRolloutPathInHome(sessionID, i.codexConversationHome()))
	return meta.ID == sessionID && meta.Cwd != "" &&
		normalizePath(meta.Cwd) == normalizePath(i.EffectiveWorkingDir()) &&
		meta.UserSession
}

// A multi-profile consumer must not use its own profile or ambient CODEX_HOME
// for a row loaded from another profile. Command and account choices remain
// more specific than the owning profile, just as in the launch resolver.
func (i *Instance) codexConversationHome() string {
	if i.storageSnapshot == nil || i.storageSnapshot.profile == "" || i.storageSnapshot.profile == resolvedProcessProfile() {
		return i.getCodexHomeDir()
	}
	if home := codexHomeFromCommand(i.resolveCodexCommand(i.Command)); home != "" {
		return home
	}
	if home := i.accountCodexHomeDir(); home != "" {
		return home
	}
	if cfg, err := LoadUserConfig(); err == nil && cfg != nil {
		if home := cfg.GetProfileCodexConfigDir(i.storageSnapshot.profile); home != "" {
			return home
		}
	}
	return i.getCodexHomeDir()
}

// validCodexHook checks the payload (or legacy anchor) before any status,
// activity, acknowledgement, generation, or binding mutation. A hook that names
// no conversation at all (a legacy notify payload with no anchor) cannot bind
// or claim a turn, so it keeps upstream's hook-over-pane status authority;
// one that carries turn evidence without an identity is rejected.
func (i *Instance) validCodexHook(status *HookStatus) bool {
	if i == nil || status == nil {
		return false
	}
	if !IsCodexCompatible(i.Tool) {
		return true
	}
	sessionID := strings.TrimSpace(status.SessionID)
	if sessionID == "" {
		sessionID = ReadHookSessionAnchor(i.ID)
	}
	if sessionID == "" {
		return status.CodexStartedGeneration == "" && status.CodexCompletedGeneration == ""
	}
	return i.ValidCodexConversationCandidate(sessionID)
}
