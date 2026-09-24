package provider

// RegistrationSentinelToken reuses the bounded Go decoder for the legacy
// registration protocol's challenge. It does not launch a browser or execute JS.
func RegistrationSentinelToken(dx, requirements string) (string, error) {
	return solveSentinelTurnstileToken(dx, requirements)
}
