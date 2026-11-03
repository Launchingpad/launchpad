package backupapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"devops/internal/backup/driver"
	"devops/internal/dbq"
	"devops/internal/platform"
)

// The destination types and their secrets (decisions-T16 #9, #24, #25).

// Destination types (the CHECK of backup_destinations.type).
const (
	TypeS3      = "s3"
	TypeWebDAV  = "webdav"
	TypeSFTP    = "sftp"
	TypeDropbox = "dropbox"
)

var knownTypes = []string{TypeS3, TypeWebDAV, TypeSFTP, TypeDropbox}

func knownType(t string) bool {
	for _, k := range knownTypes {
		if k == t {
			return true
		}
	}
	return false
}

// The secret names. One blob holds all secrets of a destination.
const (
	secretRefreshToken = "refreshToken" // Dropbox: made by offsite's exchange, never by the API
)

type secretSpec struct {
	allowed  []string // names the API accepts
	required []string // all of these must be set to enable
	oneOf    []string // at least one of these must be set to enable
}

var secretSpecs = map[string]secretSpec{
	TypeS3:      {allowed: []string{"accessKey", "secretKey"}, required: []string{"accessKey", "secretKey"}},
	TypeWebDAV:  {allowed: []string{"password"}},
	TypeSFTP:    {allowed: []string{"password", "privateKey"}, oneOf: []string{"password", "privateKey"}},
	TypeDropbox: {allowed: []string{"appKey", "appSecret", secretRefreshToken}, required: []string{"appKey", "appSecret", secretRefreshToken}},
}

const (
	maxSecretLen    = 16 << 10
	maxSecretsTotal = 64 << 10
	maxConfigLen    = 64 << 10
)

func (sp secretSpec) allows(name string) bool {
	for _, a := range sp.allowed {
		if a == name {
			return true
		}
	}
	return false
}

// checkSecretName refuses a name the type does not use. The API never lets a
// caller write the Dropbox refresh token: only "" (clear) is accepted for it.
func checkSecretName(typ, name string, value string, isSet bool) error {
	sp := secretSpecs[typ]
	if !sp.allows(name) {
		return invalid("secrets.%s is not a secret of a %s destination", name, typ)
	}
	if name == secretRefreshToken && isSet && value != "" {
		return invalid("secrets.%s is made by the Dropbox exchange; it can only be cleared here", name)
	}
	if len(value) > maxSecretLen {
		return invalid("secrets.%s is longer than %d bytes", name, maxSecretLen)
	}
	return nil
}

// missingSecrets lists the required secrets of a type that set lacks (names only).
func missingSecrets(typ string, set map[string]string) []string {
	sp := secretSpecs[typ]
	var miss []string
	for _, r := range sp.required {
		if set[r] == "" {
			miss = append(miss, r)
		}
	}
	if len(sp.oneOf) > 0 {
		ok := false
		for _, n := range sp.oneOf {
			if set[n] != "" {
				ok = true
			}
		}
		if !ok {
			miss = append(miss, strings.Join(sp.oneOf, " or "))
		}
	}
	return miss
}

func setNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// openSecrets opens a destination's blob. unreadable is true for a blob this
// server cannot open (no key, another key, a blob of another row, damage).
func (s *Service) openSecrets(d dbq.BackupDestination) (m map[string]string, unreadable bool) {
	if d.SecretsEnc == nil {
		return map[string]string{}, false
	}
	if s.keys == nil {
		return nil, true
	}
	pt, err := s.keys.Open(platform.BackupColDestinationSecrets, d.ID.String(), d.SecretsEnc)
	if err != nil {
		return nil, true
	}
	m = map[string]string{}
	if err := json.Unmarshal(pt, &m); err != nil {
		return nil, true
	}
	return m, false
}

// sealSecrets seals the secrets of a row. An empty map is no blob. The error
// never holds a value.
func (s *Service) sealSecrets(column, owner string, m map[string]string) ([]byte, error) {
	if len(setNames(m)) == 0 {
		return nil, nil
	}
	if s.keys == nil {
		return nil, errNoKeyWrite
	}
	pt, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode secrets: %w", err)
	}
	if len(pt) > maxSecretsTotal {
		return nil, invalid("the secrets together are longer than %d bytes", maxSecretsTotal)
	}
	blob, err := s.keys.Seal(column, owner, pt)
	if err != nil {
		return nil, fmt.Errorf("seal secrets: %w", err)
	}
	return blob, nil
}

// parseConfig reads a config body: a JSON object, at most 64 KiB. null is not a config.
func parseConfig(raw json.RawMessage) (map[string]any, json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil, invalid("config is required")
	}
	if len(raw) > maxConfigLen {
		return nil, nil, invalid("config is longer than %d bytes", maxConfigLen)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, nil, invalid("config must be a JSON object")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, nil, invalid("config must be a JSON object")
	}
	return m, buf.Bytes(), nil
}

// changedFields lists the top-level config keys whose value differs (names only).
func changedFields(a, b map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	for k, v := range a {
		seen[k] = true
		if w, ok := b[k]; !ok || !reflect.DeepEqual(v, w) {
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Where the trust anchor lives in config (decisions-T16 #25, #36): the SPKI pin
// of a TLS destination, or the SSH host key of an SFTP one.
const (
	fieldTLSPin  = "tlsPin"
	fieldHostKey = "hostKey"
)

func anchorField(typ string) string {
	if typ == TypeSFTP {
		return fieldHostKey
	}
	return fieldTLSPin
}

// identityDetail is the canonical (host, bucket, folder) of a config for the
// change log, or nil when the type has no driver or the config is invalid. It
// holds no credential: Identity returns the host without userinfo or query.
func identityDetail(typ string, cfg json.RawMessage) map[string]any {
	d, ok := driver.Lookup(typ)
	if !ok {
		return nil
	}
	id, err := d.Identity(cfg)
	if err != nil {
		return nil
	}
	return map[string]any{"host": id.Host, "bucket": id.Bucket, "folder": id.Folder}
}
