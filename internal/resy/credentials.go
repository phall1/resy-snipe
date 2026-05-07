package resy

import "resy-snipe/internal/domain"

// providerID is the constant ID this adapter advertises on Provider.ID()
// and the (provider, user) key it writes into store.SessionRow.
const providerID domain.ProviderID = "resy"

// Credentials is the typed login material this adapter accepts. Any
// providers.Credentials that the engine hands to Client.Login must
// have this concrete type — the type assertion in Login is the gate.
type Credentials struct {
	Email    string
	Password string
}

// Provider satisfies providers.Credentials.
func (Credentials) Provider() domain.ProviderID { return providerID }
