package backup

import (
	"encoding/json"
	"fmt"
	"time"
)

// ActiveRecipientKeys returns the age public keys offsite encrypts to at t
// (decisions-T16 #20): from backup_settings.recipients, the elements whose
// effectiveAt has passed and whose removal (removeAt) has not. A pending add and
// a key whose removal landed are left out. t is the DATABASE clock.
func ActiveRecipientKeys(recipients json.RawMessage, t time.Time) ([]string, error) {
	var all []struct {
		PublicKey   string     `json:"publicKey"`
		EffectiveAt time.Time  `json:"effectiveAt"`
		RemoveAt    *time.Time `json:"removeAt"`
	}
	if err := json.Unmarshal(recipients, &all); err != nil {
		return nil, fmt.Errorf("backup: read the recipients: %w", err)
	}
	var out []string
	for _, r := range all {
		if r.EffectiveAt.After(t) || (r.RemoveAt != nil && !r.RemoveAt.After(t)) {
			continue
		}
		out = append(out, r.PublicKey)
	}
	return out, nil
}
