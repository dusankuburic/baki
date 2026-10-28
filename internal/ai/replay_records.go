package ai

// ReplayRecordsStandAlone reports whether providerID's replay records (Message.ProviderItems) stand alone per
// message: each holds only its own turn's parts — Gemini's thought signatures, keyed by the call ids of that
// message (geminiReplayRecord) — so a history that left other messages out still replays the ones it kept, and
// must: Gemini 3 refuses a replayed function call of the current turn without its signature. Records chained
// across turns (Claude's thinking blocks, the Responses API's reasoning items) are replayed only on an
// append-only history.
func ReplayRecordsStandAlone(providerID string) bool {
	return providerID == geminiSpec.ID
}
