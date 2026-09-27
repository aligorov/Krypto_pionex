package autogrid

import "testing"

// ── v2.0.143 audit-2: Telegram channel semantics for the flip and the OFI
// emergency exits (unit half — the routing switch is pure on purpose). ──────

// TestTelegramRoutingDirectionFlipAlwaysSends pins the always-on semantics:
// a REAL direction reversal moving real money must not be silencable by ANY
// per-channel toggle — pre-fix the flip rode notify_range_adjust, so an
// operator who quieted routine range shifts also blinded themselves to
// flips.
func TestTelegramRoutingDirectionFlipAlwaysSends(t *testing.T) {
	allOff := telegramChannels{} // every toggle false, every template empty
	for name, ch := range map[string]telegramChannels{
		"all toggles off": allOff,
		"adjust off (the old carrier)": {notifyAdjust: false},
		"everything on": {notifyCreated: true, notifyTake: true, notifyStop: true,
			notifyAdjust: true, notifyDigest: true, notifyEmergency: true},
	} {
		shouldSend, tmpl := telegramEventRouting("DIRECTION_FLIP", ch)
		if !shouldSend {
			t.Errorf("%s: DIRECTION_FLIP shouldSend = false, want true (unconditional)", name)
		}
		if tmpl == "" {
			t.Errorf("%s: DIRECTION_FLIP template must be non-empty", name)
		}
	}
}

// TestTelegramRoutingEmergencyOFIRespectsToggle pins the emergency-channel
// semantics: EMERGENCY_OFI_DUMP/PUMP used to fall into the generic default
// lane (shouldSend = true) and bypassed notify_emergency entirely — with the
// toggle off they must stay silent, with it on they must carry the 🚨
// template that renders protection.go's message body.
func TestTelegramRoutingEmergencyOFIRespectsToggle(t *testing.T) {
	for _, eventType := range []string{"EMERGENCY_OFI_DUMP", "EMERGENCY_OFI_PUMP"} {
		if shouldSend, _ := telegramEventRouting(eventType, telegramChannels{notifyEmergency: false}); shouldSend {
			t.Errorf("%s: shouldSend = true with notifyEmergency=false, want false (the old default-lane bypass)", eventType)
		}
		shouldSend, tmpl := telegramEventRouting(eventType, telegramChannels{notifyEmergency: true})
		if !shouldSend {
			t.Errorf("%s: shouldSend = false with notifyEmergency=true, want true", eventType)
		}
		if tmpl == "" {
			t.Errorf("%s: template must be non-empty", eventType)
		}
	}
}
