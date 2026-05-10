package secrets

// Kind names the byte-shape of a sealed plaintext. Each Kind has a
// documented plaintext format in docs/v2/design/secrets.md §Sealing
// primitive; adding a Kind is an ADR (because the byte-shape contract
// needs to be written down where reviewers can find it).
type Kind string

const (
	// KindResyPassword is the UTF-8 of the user-typed Resy password,
	// no trimming.
	KindResyPassword Kind = "resy_password"

	// KindResySessionJWT is the canonical JSON of a Resy session JWT
	// blob (the single string field in domain.SessionRow.JWT).
	KindResySessionJWT Kind = "resy_session_jwt"
)

// Valid reports whether k is a known Kind. Unknown kinds are an error
// at Seal time so a typo never makes it onto disk.
func (k Kind) Valid() bool {
	switch k {
	case KindResyPassword, KindResySessionJWT:
		return true
	}
	return false
}
