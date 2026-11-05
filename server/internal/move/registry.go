package move

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"devops/internal/platform"
)

// Mode says what the bundle does with a table.
type Mode int

const (
	// ModeCopy: the rows travel as they are.
	ModeCopy Mode = iota
	// ModeCopyReseal: the rows travel and sealed columns are opened on export
	// and sealed again on import (the bundle holds their plaintext only
	// inside the passphrase-encrypted stream).
	ModeCopyReseal
	// ModeSkip: the table does not travel. Import leaves it as the migrations
	// made it.
	ModeSkip
)

// Keyring names, for the Sealed.Ring field.
const (
	ringSettings = "settings" // platform.Keyring (platform_settings)
	ringGit      = "git"      // platform.GitKeyring
	ringPush     = "push"     // platform.PushKeyring
	ringSlack    = "slack"    // platform.SlackKeyring (chat services and personal webhooks)
	ringBackup   = "backup"   // platform.BackupKeyring: the ONLY keyring for a backup secret
)

// Sealed is one sealed (*_enc) column.
type Sealed struct {
	Column string
	Ring   string
	// KeyringColumn is the column name the keyring binds the blob to, when it
	// differs from Column (push, chat, backup use a qualified name).
	KeyringColumn string
	// Scope returns the owner, row id or scope string the keyring binds the
	// blob to. Nil for the settings ring (column only).
	Scope func(r *row) string
}

func (s Sealed) aadColumn() string {
	if s.KeyringColumn != "" {
		return s.KeyringColumn
	}
	return s.Column
}

// Deferred is a nullable column of a cycle in the foreign keys: import writes
// the rows with the column NULL, then sets it once every table is in.
type Deferred struct {
	Column string
	Ref    string // the table the column points at
}

// Table is the registry entry of one table.
type Table struct {
	Name string
	Mode Mode
	// Why is a short reason, mainly for skip and the odd copy.
	Why string
	// Where limits the exported rows (a SQL condition on the table).
	Where string
	// Sealed lists every *_enc column of the table (ModeCopyReseal).
	Sealed []Sealed
	// ClearFirst: import deletes the rows the migrations or a trigger made
	// before it copies (a singleton row, rows seeded for a new org).
	ClearFirst bool
	Deferred   []Deferred
	// OnImport changes a row after it is sealed again (fresh ids, switches off).
	OnImport func(ic *importCtx, r *row) error
}

func orgScope(r *row) string  { return r.text("org_id") }
func idScope(r *row) string   { return r.text("id") }
func sealScope(r *row) string { return r.text("seal_scope") }

// Registry is every table of the schema. TestRegistryCoversSchema fails when
// a table, or an *_enc column of a copied table, is missing.
var Registry = []Table{
	{Name: "agent_guidance", Mode: ModeCopy},
	{Name: "api_tokens", Mode: ModeCopy, Where: "kind <> 'session'",
		Why: "token hashes travel so agents and scripts keep working; web sessions (kind session) do not"},
	{Name: "attachments", Mode: ModeCopy, Why: "rows; the files travel as objects"},
	{Name: "backup_change_log", Mode: ModeSkip, Why: "audit log of the old install's backup settings; new install starts its own"},
	{Name: "backup_destinations", Mode: ModeCopyReseal,
		Sealed: []Sealed{{Column: "secrets_enc", Ring: ringBackup, KeyringColumn: platform.BackupColDestinationSecrets, Scope: idScope}},
		Why:    "destinations come across DISABLED (a clone must not write to the old install's backup target by surprise)",
		OnImport: func(ic *importCtx, r *row) error {
			r.setText("enabled", "f")
			r.setNull("enabled_since")
			r.setNull("last_stale_alert_at")
			return nil
		}},
	{Name: "backup_dumps", Mode: ModeSkip, Why: "dump files live on the old host"},
	{Name: "backup_requests", Mode: ModeSkip, Why: "queue between the app and the uploader"},
	{Name: "backup_runs", Mode: ModeSkip, Why: "run history of the old install; includes test_secrets_enc of unsaved tests, which never travel"},
	{Name: "backup_settings", Mode: ModeCopyReseal, ClearFirst: true,
		Sealed: []Sealed{{Column: "signing_private_enc", Ring: ringBackup, KeyringColumn: platform.BackupColSigningKey,
			Scope: func(*row) string { return platform.BackupSettingsOwner }}},
		Why: "singleton; recipients and the signing key travel; install_id and instance_id are new (the new install is a new backup source)",
		OnImport: func(ic *importCtx, r *row) error {
			id := make([]byte, 4)
			if _, err := rand.Read(id); err != nil {
				return err
			}
			r.setText("install_id", hex.EncodeToString(id))
			r.setText("instance_id", uuid.NewString())
			if !r.isNull("key_check") {
				if ic.keys.Backup == nil {
					r.setNull("key_check")
				} else {
					r.setBytea("key_check", ic.keys.Backup.KeyCheck())
				}
			}
			return nil
		}},
	{Name: "chat_mentions", Mode: ModeCopy, Why: "dedupe memory of posted mentions"},
	{Name: "chat_outbox", Mode: ModeSkip, Why: "pending posts would be sent twice"},
	{Name: "chat_services", Mode: ModeCopyReseal,
		Sealed: []Sealed{{Column: "webhook_enc", Ring: ringSlack, KeyringColumn: platform.SlackColWebhook, Scope: sealScope}}},
	{Name: "comments", Mode: ModeCopy},
	{Name: "cycle_snapshots", Mode: ModeCopy},
	{Name: "cycles", Mode: ModeCopy},
	{Name: "documents", Mode: ModeCopy},
	{Name: "events", Mode: ModeCopy, Why: "the change feed and its history"},
	{Name: "git_identities", Mode: ModeCopy},
	{Name: "git_integrations", Mode: ModeCopyReseal, Sealed: []Sealed{
		{Column: "token_enc", Ring: ringGit, Scope: idScope},
		{Column: "webhook_secret_enc", Ring: ringGit, Scope: idScope},
		{Column: "webhook_secret_next_enc", Ring: ringGit, Scope: idScope},
	}},
	{Name: "git_repo_map", Mode: ModeCopy},
	{Name: "git_webhook_deliveries", Mode: ModeCopy},
	{Name: "git_webhook_events", Mode: ModeCopy},
	{Name: "idempotency_keys", Mode: ModeSkip, Why: "replay cache of responses; short-lived"},
	{Name: "import_jobs", Mode: ModeSkip, Why: "Linear import work state, tied to the old host's files"},
	{Name: "import_mappings", Mode: ModeCopy, Why: "keeps a later Linear re-import idempotent"},
	{Name: "import_sessions", Mode: ModeSkip, Why: "Linear import staging, a path on the old host"},
	{Name: "issue_activity", Mode: ModeCopy},
	{Name: "issue_counters", Mode: ModeCopy},
	{Name: "issue_labels", Mode: ModeCopy},
	{Name: "issue_relations", Mode: ModeCopy},
	{Name: "issue_state_changes", Mode: ModeCopy},
	{Name: "issue_subscriptions", Mode: ModeCopy},
	{Name: "issues", Mode: ModeCopy},
	{Name: "labels", Mode: ModeCopy},
	{Name: "login_codes", Mode: ModeSkip, Why: "one-time credentials"},
	{Name: "milestones", Mode: ModeCopy},
	{Name: "mr_links", Mode: ModeCopy},
	{Name: "notification_cursors", Mode: ModeCopy, ClearFirst: true,
		Why: "where the notify consumer stopped; a trigger seeds a row per org, import replaces it"},
	{Name: "notification_emails", Mode: ModeSkip, Why: "pending mail would be sent twice"},
	{Name: "notification_mail_tokens", Mode: ModeCopy, Why: "sha256 of unsubscribe tokens; links in old mail keep working"},
	{Name: "notification_settings", Mode: ModeCopy},
	{Name: "notification_user_settings", Mode: ModeCopy},
	{Name: "notifications", Mode: ModeCopy},
	{Name: "onboarding_skips", Mode: ModeCopy},
	{Name: "org_event_counters", Mode: ModeCopy},
	{Name: "org_invite_teams", Mode: ModeCopy},
	{Name: "org_invites", Mode: ModeCopy, Why: "token hashes only; open invites stay open"},
	{Name: "org_members", Mode: ModeCopy},
	{Name: "orgs", Mode: ModeCopy, Deferred: []Deferred{{Column: "default_team_id", Ref: "teams"}}},
	{Name: "personal_webhook_outbox", Mode: ModeSkip, Why: "pending posts would be sent twice"},
	{Name: "personal_webhooks", Mode: ModeCopyReseal,
		Sealed: []Sealed{{Column: "webhook_enc", Ring: ringSlack, KeyringColumn: platform.PersonalColWebhook, Scope: sealScope}}},
	{Name: "platform_settings", Mode: ModeCopyReseal, ClearFirst: true,
		Sealed: []Sealed{
			{Column: "postmark_token_enc", Ring: ringSettings},
			{Column: "smtp_password_enc", Ring: ringSettings},
		},
		Why: "singleton; key_check becomes the new server's",
		OnImport: func(ic *importCtx, r *row) error {
			if !r.isNull("key_check") {
				if ic.keys.Settings == nil {
					r.setNull("key_check")
				} else {
					r.setBytea("key_check", ic.keys.Settings.KeyCheck())
				}
			}
			return nil
		}},
	{Name: "product_teams", Mode: ModeCopy},
	{Name: "products", Mode: ModeCopy},
	{Name: "project_label_assignments", Mode: ModeCopy},
	{Name: "project_labels", Mode: ModeCopy},
	{Name: "project_statuses", Mode: ModeCopy, ClearFirst: true, Why: "a trigger seeds six statuses per new org; import replaces them with the exported ones"},
	{Name: "project_teams", Mode: ModeCopy},
	{Name: "project_updates", Mode: ModeCopy},
	{Name: "projects", Mode: ModeCopy},
	{Name: "push_devices", Mode: ModeCopyReseal,
		Sealed: []Sealed{{Column: "token_enc", Ring: ringPush, KeyringColumn: platform.PushColDeviceToken,
			Scope: func(r *row) string { return r.text("org_id") + "." + r.text("id") }}}},
	{Name: "push_outbox", Mode: ModeSkip, Why: "pending pushes would be sent twice"},
	{Name: "push_relays", Mode: ModeCopyReseal, Sealed: []Sealed{
		{Column: "url_enc", Ring: ringPush, KeyringColumn: platform.PushColRelayURL, Scope: orgScope},
		{Column: "secret_enc", Ring: ringPush, KeyringColumn: platform.PushColRelaySecret, Scope: orgScope},
	}},
	{Name: "setup_tokens", Mode: ModeSkip, Why: "one-time credentials; import deletes the open ones, setup is over"},
	{Name: "signup_requests", Mode: ModeCopy, Why: "pending and decided sign-ups; code hashes expire on their own"},
	{Name: "team_follows", Mode: ModeCopy},
	{Name: "team_git_rule_from_states", Mode: ModeCopy},
	{Name: "team_git_rules", Mode: ModeCopy},
	{Name: "team_members", Mode: ModeCopy},
	{Name: "teams", Mode: ModeCopy, Deferred: []Deferred{
		{Column: "default_state_id", Ref: "workflow_states"},
		{Column: "review_state_id", Ref: "workflow_states"},
		{Column: "testing_state_id", Ref: "workflow_states"},
	}},
	{Name: "user_preferences", Mode: ModeCopy},
	{Name: "user_view_preferences", Mode: ModeCopy},
	{Name: "users", Mode: ModeCopy},
	{Name: "workflow_states", Mode: ModeCopy},
}

// Sequences are the sequences a copied table fills; import moves each one past
// the copied values. TestRegistryCoversSchema fails when the schema has a
// sequence that is not here.
var Sequences = []struct{ Name, Table, Column string }{
	{"notifications_inbox_seq_seq", "notifications", "inbox_seq"},
}

// ObjectSources are the columns that name an object in the store. The bundle
// carries the objects the snapshot rows name, never a bucket listing.
var ObjectSources = []struct{ Table, KeyColumn, TypeColumn, Where string }{
	{"attachments", "object_key", "content_type", "kind = 'file' AND object_key IS NOT NULL"},
	{"orgs", "avatar_key", "avatar_content_type", "avatar_key IS NOT NULL"},
	{"projects", "avatar_key", "avatar_content_type", "avatar_key IS NOT NULL"},
}

func registryTable(name string) (Table, bool) {
	for _, t := range Registry {
		if t.Name == name {
			return t, true
		}
	}
	return Table{}, false
}

func (t Table) copied() bool { return t.Mode != ModeSkip }

// sortedCopied orders the copied tables so every table comes after the tables
// its foreign keys point at (Kahn, ties by name). deps maps table to the
// tables it references (from pg_constraint). Edges of Deferred columns and
// edges to skipped tables are dropped.
func sortedCopied(deps map[string][]string) ([]Table, error) {
	copied := map[string]Table{}
	for _, t := range Registry {
		if t.copied() {
			copied[t.Name] = t
		}
	}
	skipEdge := map[[2]string]bool{}
	for _, t := range copied {
		for _, d := range t.Deferred {
			skipEdge[[2]string{t.Name, d.Ref}] = true
		}
	}
	indeg := map[string]int{}
	children := map[string][]string{}
	for name := range copied {
		indeg[name] += 0
	}
	for name, refs := range deps {
		if _, ok := copied[name]; !ok {
			continue
		}
		seen := map[string]bool{}
		for _, ref := range refs {
			if _, ok := copied[ref]; !ok || ref == name || seen[ref] || skipEdge[[2]string{name, ref}] {
				continue
			}
			seen[ref] = true
			indeg[name]++
			children[ref] = append(children[ref], name)
		}
	}
	var ready []string
	for name, d := range indeg {
		if d == 0 {
			ready = append(ready, name)
		}
	}
	var out []Table
	for len(ready) > 0 {
		sort.Strings(ready)
		n := ready[0]
		ready = ready[1:]
		out = append(out, copied[n])
		for _, c := range children[n] {
			if indeg[c]--; indeg[c] == 0 {
				ready = append(ready, c)
			}
		}
	}
	if len(out) != len(copied) {
		return nil, fmt.Errorf("move: the foreign keys of the copied tables form a cycle the registry does not defer")
	}
	return out, nil
}
