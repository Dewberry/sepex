package handlers

import (
	"app/utils"
	"strings"
)

// ExecutionModeResult contains the determined execution mode and whether
// a Prefer header preference was honored (for Preference-Applied response header)
type ExecutionModeResult struct {
	Mode              string // "sync-execute" or "async-execute"
	PreferenceApplied string // Value for Preference-Applied header, empty if none
}

// DetermineExecutionMode implements OGC API - Processes execution mode logic
// per Requirements 25 and 26, and Recommendation 12A.
//
// Decision matrix:
//   - No Prefer header + async-only process  → async (Req 25A)
//   - No Prefer header + sync-only process   → sync  (Req 25B)
//   - No Prefer header + both modes          → sync  (Req 25C - default)
//   - Prefer: respond-async + async-only     → async (Req 26A)
//   - Prefer: respond-async + sync-only      → sync  (Req 26B - ignore preference)
//   - Prefer: respond-async + both modes     → async (Req 26C + Rec 12A - honor preference)
func DetermineExecutionMode(jobControlOptions []string, preferHeader string) ExecutionModeResult {
	supportsSync := utils.StringInSlice("sync-execute", jobControlOptions)
	supportsAsync := utils.StringInSlice("async-execute", jobControlOptions)
	wantsAsync := parseRespondAsyncPreference(preferHeader)

	result := ExecutionModeResult{}

	// Case 1: Process only supports async (Req 25A, 26A)
	if supportsAsync && !supportsSync {
		result.Mode = "async-execute"
		return result
	}

	// Case 2: Process only supports sync (Req 25B, 26B)
	if supportsSync && !supportsAsync {
		result.Mode = "sync-execute"
		// If client requested async but we can only do sync, no preference was applied
		return result
	}

	// Case 3: Process supports both modes
	if wantsAsync {
		// Req 26C + Rec 12A: Honor the async preference
		result.Mode = "async-execute"
		result.PreferenceApplied = "respond-async"
		return result
	}

	// Req 25C: Default to sync when no preference given
	result.Mode = "sync-execute"
	return result
}

// parseRespondAsyncPreference checks if the Prefer header contains "respond-async"
// The Prefer header can contain multiple comma or space separated preferences.
// Example: "respond-async, wait=10" or "respond-async"
func parseRespondAsyncPreference(preferHeader string) bool {
	if preferHeader == "" {
		return false
	}

	// Prefer header values can be comma-separated
	// Each preference can have parameters separated by semicolons
	// We're looking for "respond-async" token
	preferences := strings.Split(preferHeader, ",")
	for _, pref := range preferences {
		// Trim whitespace and get the preference name (before any parameters)
		pref = strings.TrimSpace(pref)
		// Handle parameters like "respond-async; wait=10"
		parts := strings.SplitN(pref, ";", 2)
		prefName := strings.TrimSpace(parts[0])

		if prefName == "respond-async" {
			return true
		}
	}

	return false
}
