package upstream

// IsModelAccessDenied identifies the provider's explicit account entitlement
// rejection. It is not a request-shape or output-budget error, and healthy
// accounts must not be penalized or retried under a different model.
func IsModelAccessDenied(body string) bool {
	return upstreamErrorCode(body) == 11158
}
