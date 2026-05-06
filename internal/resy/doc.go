// Package resy is the Resy adapter implementing providers.Provider. It
// owns the typed HTTP client, auth/MFA flow, session persistence hooks,
// and the find/details/book pipeline. All Resy-specific behaviour —
// including User-Agent and origin spoofing, anti-bot pacing, and response
// classification — is contained in this package.
package resy
