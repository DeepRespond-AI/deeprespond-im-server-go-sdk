// Package drim provides the DeepRespond IM server SDK for application-level
// OpenAPI calls, user management, messaging, and login-ticket issuance.
// Reuse one Client across goroutines; it manages application tokens and retries.
// The callback subpackage verifies and dispatches signed server callbacks.
package drim
