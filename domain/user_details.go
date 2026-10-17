package domain

// UserDetails is what a person has told us about themselves, so agents can address
// them properly. Every field is empty until they say.
type UserDetails struct {
	Name     string // what to call them
	Timezone string // IANA name, e.g. Africa/Lagos
	Language string // BCP-47 tag, e.g. fr or pt-BR: what to reply in
	Country  string // ISO 3166-1 alpha-2, e.g. NG: where they live
	City     string
}
