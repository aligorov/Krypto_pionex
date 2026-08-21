package autogrid

// cooldownHours escalates the per-symbol protective-close cooldown window
// (v2.0.28). The flat 2h window let a pair stop out, wait 2 hours, re-enter
// the same trend signal near the channel top and stop AGAIN — double stops
// on VIRTUAL/NEAR (2026-08-20 night) and the CRWVX morning loop. Every
// additional protective close in the trailing 24h doubles the window:
// 1 close → 2h, 2 → 4h, 3 → 8h, 4 → 16h, 5+ → 24h saturation. A tape that
// keeps killing bots must stay closed longer than one that died once.
func cooldownHours(protectiveCloses int) int {
	if protectiveCloses <= 1 {
		return 2
	}
	hours := 2 << (protectiveCloses - 1) // 2 → 4 → 8 → 16
	if hours > 24 {
		return 24
	}
	return hours
}
