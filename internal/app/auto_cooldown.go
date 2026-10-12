package app

import "ccLoad/internal/model"

const autoCooldownEnabledSettingKey = "cooldown_enabled"

// Like the other cooldown settings, this is a startup setting. Saving it through
// the admin API uses the normal restart flow so all readers switch together.
func (s *Server) autoCooldownEnabled() bool {
	return s.configService == nil || s.configService.GetBool(autoCooldownEnabledSettingKey, true)
}

// Cached keys are shared read-only snapshots. Copy before masking old cooldowns
// so disabling the policy neither mutates the cache nor deletes persisted state.
func (s *Server) keysWithEffectiveCooldowns(keys []*model.APIKey) []*model.APIKey {
	if s.autoCooldownEnabled() || len(keys) == 0 {
		return keys
	}
	out := make([]*model.APIKey, len(keys))
	for i, key := range keys {
		out[i] = key
		if key != nil && (key.CooldownUntil != 0 || key.CooldownDurationMs != 0) {
			copy := *key
			copy.CooldownUntil = 0
			copy.CooldownDurationMs = 0
			out[i] = &copy
		}
	}
	return out
}
