package server

// A system instruction has no dedicated wire field in the official body, so it is
// carried as leading text in the user turn. It must reach the provider rather
// than be rejected: rejecting it made every client that always sends a system
// prompt unusable. See TestProviderSystemInstructionsAreCarriedAsText.
