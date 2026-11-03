#!/usr/bin/env bash
# audit-public.sh DIR [--gitleaks | --identity-only]
#
# Greps a tree that is about to become public for values that belong to the
# author's own setup or look like a secret, and checks its file names, its
# binary files and its paths. Exit 0: no hit. Exit 1: hits (each one is
# printed with its file and line). Exit 2: usage error. It reads the tree
# only; it never changes a file, runs no network call and pushes nothing.
#
# --gitleaks also runs gitleaks (pinned docker image, --no-git) on DIR. That
# needs docker and the image; without docker the option is an error.
#
# When DIR is a git repository, the author, committer and message of every
# commit, and the tagger and message of every annotated tag, are checked too.
# --identity-only runs only that check (export-public.sh runs it after its
# commit); without a git repository in DIR it is exit 2.
#
# Every content check reads every file as bytes (grep -a, LC_ALL=C), binary
# or not, so a NUL or a Latin-1 byte cannot hide a line. Only the allowed
# binary files (fonts, README screenshots) are left out of the content checks,
# and those must pass a structure check of their own.
#
# Allowlists are in this file on purpose, next to the pattern they relax.
set -euo pipefail

GITLEAKS_IMAGE="${GITLEAKS_IMAGE:-zricethezav/gitleaks:v8.24.2}"

dir="${1:-}"
[[ -n "$dir" && -d "$dir" ]] || {
	echo "usage: audit-public.sh DIR [--gitleaks | --identity-only]" >&2
	exit 2
}
shift
run_gitleaks=0
identity_only=0
for a in "$@"; do
	case "$a" in
	--gitleaks) run_gitleaks=1 ;;
	--identity-only) identity_only=1 ;;
	*)
		echo "audit-public.sh: unknown argument '$a'" >&2
		exit 2
		;;
	esac
done

cd "$dir"
hits=0
# Bytes, not characters: a file that is not valid UTF-8 is still read, and
# the case folding of -i is plain ASCII.
export LC_ALL=C

# The author's other private repositories. Built from parts so this file does
# not spell them.
# Any separator (or none) between the two words: launchpad-apps,
# launchpad_apps, "launchpad apps".
private_repos="launch""pad[-_ .]?(ap""ps|rel""ay|si""te)|out""post"
# The author's first name as a word. Letters bound it, not \b: an underscore
# or a digit is a boundary here (chris_notes, chris2).
first_name='(^|[^A-Za-z])chris([^A-Za-z]|$)'
# Words that must not appear in a file name or in a commit identity.
ident_re="mintywater|yourorbit|odyssey|kidchat|macmini|chriswunsch|wunsch|gmail\.com|$first_name|$private_repos"

# The author, the committer and the message (with its trailers) of every
# commit, when DIR is a git repository. A hit names the commit and the
# field, never the value. A git error is exit 2: the check never passes
# when it could not run.
identity_check() {
	if [[ ! -e .git ]]; then
		if ((identity_only)); then
			echo "audit-public.sh: $dir is not a git repository; the commit identities were not checked" >&2
			exit 2
		fi
		return 0
	fi
	local commits c field fmt text out=""
	# Read DIR's own repository, never one named by the caller's environment.
	unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_CEILING_DIRECTORIES
	if ! commits="$(git rev-list --all 2>/dev/null)"; then
		echo "audit-public.sh: git cannot read $dir; the commit identities were not checked" >&2
		exit 2
	fi
	for c in $commits; do
		for field in author committer message; do
			case "$field" in
			author) fmt='%an <%ae>' ;;
			committer) fmt='%cn <%ce>' ;;
			message) fmt='%B' ;;
			esac
			if ! text="$(git show -s --format="$fmt" "$c" 2>/dev/null)"; then
				echo "audit-public.sh: git cannot read commit ${c:0:12}; the commit identities were not checked" >&2
				exit 2
			fi
			# A here-string, not a pipe: grep -q stops early, and under
			# pipefail the writer's SIGPIPE would turn a hit into a pass.
			if grep -qaiE "$ident_re" <<<"$text"; then
				out+="${c:0:12} $field"$'\n'
			fi
		done
	done
	local tags t
	if ! tags="$(git for-each-ref --format='%(objecttype) %(refname)' refs/tags 2>/dev/null)"; then
		echo "audit-public.sh: git cannot read the tags of $dir; the identities were not checked" >&2
		exit 2
	fi
	while read -r field t; do
		[[ "$field" == tag ]] || continue
		for field in tagger message; do
			case "$field" in
			tagger) fmt='%(taggername) %(taggeremail)' ;;
			message) fmt='%(contents)' ;;
			esac
			if ! text="$(git for-each-ref --format="$fmt" "$t" 2>/dev/null)"; then
				echo "audit-public.sh: git cannot read tag ${t#refs/tags/}; the identities were not checked" >&2
				exit 2
			fi
			if grep -qaiE "$ident_re" <<<"$text"; then
				out+="tag ${t#refs/tags/} $field"$'\n'
			fi
		done
	done <<<"$tags"
	if [[ -n "$out" ]]; then
		echo "== HIT: commit or tag identity or message names the author's host or name"
		printf '%s' "$out"
		hits=$((hits + 1))
	fi
}

if ((identity_only)); then
	identity_check
	if ((hits > 0)); then
		echo "audit-public.sh: $hits check(s) with hits"
		exit 1
	fi
	echo "audit-public.sh: no hits (identity)"
	exit 0
fi

# A check that cannot run (an unreadable file, a grep or perl error) must
# never look like a pass: fail2 records it, and the audit then ends with
# exit 2 ("NOT audited"), whatever the other checks found.
work="$(mktemp -d "${TMPDIR:-/tmp}/audit-public.XXXXXX")" || {
	echo "audit-public.sh: cannot make a scratch folder" >&2
	exit 2
}
trap 'rm -rf "$work"' EXIT
failed="$work/failed"
fail2() { printf '%s\n' "$1" >>"$failed"; }
end_if_failed() {
	if [[ -s "$failed" ]]; then
		echo "audit-public.sh: a check could not run, so $dir is NOT audited:" >&2
		cat "$failed" >&2
		exit 2
	fi
}

# Every path must be readable and every folder searchable: a file the checks
# cannot read would otherwise pass them unread.
all_paths="$(find . -path ./.git -prune -o -print 2>"$work/find.err")" || fail2 "find failed: $(head -3 "$work/find.err")"
[[ -s "$work/find.err" ]] && fail2 "find: $(head -3 "$work/find.err")"
unread="$(perl -nle 'next unless length; print if !-l $_ && (!-r $_ || (-d $_ && !-x $_))' <<<"$all_paths")" ||
	fail2 "perl failed while checking the permissions"
[[ -n "$unread" ]] && fail2 "paths that cannot be read: $(head -5 <<<"$unread" | tr '\n' ' ')"
end_if_failed

# The binary files that may ship (one folder each, no subfolders). Their
# bytes are checked by structure below, not by the text checks.
allowed_bin_path='^(web/src/assets/fonts/[A-Za-z0-9._-]+\.woff2|docs/screenshots/[a-z0-9-]+\.png)$'
allowed_bin_line='^(web/src/assets/fonts/[A-Za-z0-9._-]+\.woff2|docs/screenshots/[a-z0-9-]+\.png):'

# tgrep GREP-OPTIONS...: grep every file of the tree as bytes (-a), never
# .git or node_modules (a hit of its own), and drop the lines of the allowed
# binary files. Lines start with the path (no "./").
# A grep error (exit 2 or a message) is recorded with fail2.
tgrep() {
	local out rc=0 err
	err="$(mktemp "$work/grep.XXXXXX")"
	out="$(grep -ra --exclude-dir=.git --exclude-dir=node_modules "$@" . 2>"$err")" || rc=$?
	if ((rc > 1)) || [[ -s "$err" ]]; then
		fail2 "grep $* failed (exit $rc): $(head -3 "$err")"
	fi
	[[ -n "$out" ]] || return 0
	printf '%s\n' "$out" | sed 's#^\./##' | grep -aEv "$allowed_bin_line" || true
}

# check NAME REGEX [ALLOW_PATH_REGEX [ALLOW_LINE_REGEX [i]]]: every matching
# line outside the allowed paths (and not matching the allowed line text) is a
# hit. A fifth argument "i" matches case-insensitively. Every file is read as
# bytes; the .git folder is never read.
check() {
	local name="$1" re="$2" allow="${3:-^$}" allowline="${4:-^$}" icase="${5:-}" out flags=-nE
	[[ "$icase" == i ]] && flags=-nEi
	out="$(tgrep "$flags" -e "$re" |
		grep -aEv "^scripts/audit-public\.sh:" | grep -aEv "$allow" | grep -aEv "$allowline" || true)"
	if [[ -n "$out" ]]; then
		echo "== HIT: $name"
		printf '%s\n' "$out" | cut -c1-200
		hits=$((hits + 1))
	fi
}

# Author hosts, machines, personal names, private project names.
check "author hosts and machines" 'mintywater|yourorbit|odyssey|kidchat|192\.168\.1\.59|macmini' '^$' '^$' i
check "personal names and paths" '/Volumes/|chriswunsch|wunsch|gmail\.com' '^LICENSE:' '^$' i
# A macOS home folder. Case-sensitive on purpose: the API path /users/{user}
# is not one. Any first letter of the name counts.
check "macOS home folders" '/Users/[A-Za-z]' '^LICENSE:'
check "the author's first name" "$first_name" '^LICENSE:' '^$' i
check "private GitLab group or CI secrets" 'orbit/(kidchat|launchpad)|DEEPSEEK_API_KEY|ORBIT_REVIEW_TOKEN|ORBIT_CI_REF|gitlab\.mintywater' '^$' '^$' i
check "names of private repositories" "$private_repos" '^$' '^$' i

# File and folder names, with the same words. (node_modules is a hit of its
# own below and is not walked.)
names="$(find . -path ./.git -prune -o -name node_modules -prune -o -print 2>/dev/null | sed 's#^\./##' | grep -iE "$ident_re" || true)"
if [[ -n "$names" ]]; then
	echo "== HIT: file or folder names"
	printf '%s\n' "$names" | cut -c1-200
	hits=$((hits + 1))
fi

# Private key blocks and well-known token shapes.
check "private key block" '-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----'
# AKIAIOSFODNN7EXAMPLE is AWS's documented example key (the SigV4 test vector);
# glpat-planted-token-value-never-seen-again is a planted marker of a test.
check "token shapes" 'ghp_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|glpat-[A-Za-z0-9_-]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|sk-[A-Za-z0-9]{32,}|odt_[A-Za-z0-9]{43}' \
	'AKIAIOSFODNN7EXAMPLE|glpat-planted-token-value-never-seen-again'

# More token shapes. Test files are scanned too.
check "token shapes (Google key, Stripe live key)" 'AIza[0-9A-Za-z_-]{35}|sk_live_[0-9A-Za-z]+'
# npm and yarn registry credentials (.npmrc, .yarnrc.yml syntax), in any
# file. Case-sensitive: npm keys are lower case, so POSTGRES_PASSWORD= is not one.
check "npm or yarn registry credentials" '(^|[[:space:]:/])(_authToken|_auth|_password)[[:space:]]*=|npmAuth(Token|Ident)[[:space:]]*:'

# Slack incoming webhooks. The tests and the help texts use fake ids. Only
# these exact fake forms are blanked before the check, so a real webhook on
# the same line still fires: T000n/B000n (also with an encoded slash), T1/B1/,
# T9999/B9999/, the placeholder T.../B.../, the "../" path test, and the bare
# prefix with no id (followed by a space or an ellipsis, in help text).
slack_fake='hooks\.slack\.com/services/(T000[0-9](/|%2[fF])B000[0-9]|T1/B1/|T9999/B9999/|T\.\.\./B\.\.\./|\.\./B000[0-9]/| |…)'
slack="$(tgrep -nE -e 'hooks\.slack\.com/services/' | grep -aEv '^scripts/audit-public\.sh:' |
	sed -E "s#${slack_fake}#fake-slack-hook#g" | grep -aE 'hooks\.slack\.com/services/' || true)"
if [[ -n "$slack" ]]; then
	echo "== HIT: Slack webhook URL"
	printf '%s\n' "$slack" | cut -c1-200
	hits=$((hits + 1))
fi

# Credentials inside a URL (scheme://user:password@host). The allow list below
# is the complete set of fixture values the tree uses: the compose defaults
# (devops:devops, the ${POSTGRES_PASSWORD} reference, devops:x), the tests'
# short fake pairs (user:pw, u:p), the "host spoofing" test input that ends in
# @evil.example, and the hosts example.com, localhost and 127.0.0.1.
url_cred_allow='://devops:(devops|x|\$\{POSTGRES_PASSWORD\})@|://(user:pw|u:p)@|://files\.example:9443@evil\.example|@(([a-z0-9.-]+\.)?example\.com|localhost|127\.0\.0\.1)([:/ ]|$)'
check "credentials inside a URL" '://[^/@ "'"'"'`]+:[^/@ "'"'"'`]+@' '^package-lock' "$url_cred_allow"

# Ticket ids of the private tracker. Shipped migration comments, the decision
# records and a few test fixtures keep them on purpose. ORB-9999 is the
# missing issue of the delete test (the fixture team is ORB): in that file
# only, that one id is blanked and the line is checked again, so another id
# on the same line still fires.
orb_allow='^(server/db/migrations/|docs/internal/|server/internal/(importer|importjob)/[a-z_]*_test\.go|server/internal/importer/fixture_test\.go|server/internal/importjob/fixture_test\.go|server/internal/scm/scm_test\.go|server/internal/tracker/naming_test\.go|server/internal/api/manual_order_integration_test\.go|server/internal/mcp/mcp_integration_test\.go|web/src/features/settings/platformAdmins\.test\.tsx)'
orb="$(tgrep -nE -e 'ORB-[0-9]{4,}' | grep -aEv '^scripts/audit-public\.sh:' | grep -aEv "$orb_allow" |
	sed -E '/^server\/internal\/api\/delete_issue_integration_test\.go:/ { s/ORB-9999([^0-9])/\1/g; s/ORB-9999$//; }' |
	grep -aE 'ORB-[0-9]{4,}' || true)"
if [[ -n "$orb" ]]; then
	echo "== HIT: private ticket ids (ORB-nnnn)"
	printf '%s\n' "$orb" | cut -c1-200
	hits=$((hits + 1))
fi

# Email addresses: only example domains and the project's own noreply are fine.
# pw@hooks.slack.com is the user-info case of the chat URL tests
# (https://user:pw@hooks.slack.com/...), not an address.
emails="$(tgrep -oE -e '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}' | sed -E 's/^[^:]*://' |
	grep -aviE '@([a-z0-9.-]*\.)?(example\.(com|org|net|test)[a-z]*|example\.co\.uk|example|openssh\.com|e\.com|b\.co|e\.mail\.co|test|invalid|localhost|anthropic\.com|users\.noreply\.github\.com|github\.com|apache\.org|contributor-covenant\.org)$' |
	grep -avxF 'pw@hooks.slack.com' |
	sort -u || true)"
if [[ -n "$emails" ]]; then
	# Package-style names (npm scopes such as @types/node) are not addresses;
	# only a name before the @ sign is. Show the file of each remaining one.
	left=""
	while IFS= read -r e; do
		[[ -n "$e" ]] || continue
		f="$(grep -rlaF --exclude-dir=.git --exclude-dir=node_modules -e "$e" . 2>/dev/null | head -3 | tr '\n' ' ')"
		left+="$e  ($f)"$'\n'
	done <<<"$emails"
	if [[ -n "$left" ]]; then
		echo "== HIT: email addresses outside example domains"
		printf '%s' "$left" | cut -c1-200
		hits=$((hits + 1))
	fi
fi

# IPv4 addresses. Loopback, unspecified, broadcast, multicast and the
# documentation ranges pass. A private, shared (100.64/10), link-local or
# low-numbered address (every part 30 or less, such as a version number)
# passes only when it is one of the exact example values below, which the
# tests and docs use today; a new one is a hit to review (the author's own
# LAN uses private addresses too).
ip_examples=" 10.0.0.0 10.0.0.1 10.0.0.2 10.0.0.5 10.1.0.0 10.1.1.1 10.1.2.3 10.8.1.1 10.9.0.0 10.9.1.1 10.9.9.1 10.9.9.9 10.20.30.40 010.0.0.1 \
172.16.0.0 172.16.0.1 172.16.0.9 172.17.0.0 172.17.0.1 172.17.0.2 172.17.0.7 172.18.0.0 172.18.0.1 172.18.0.3 172.18.0.5 \
172.20.0.0 172.20.0.1 172.20.0.7 172.20.0.9 172.20.3.3 172.20.255.254 172.21.0.3 172.21.0.200 172.22.0.1 172.22.0.9 172.31.0.1 172.31.255.255 \
192.168.0.0 192.168.0.1 192.168.0.5 192.168.0.9 192.168.1.0 192.168.1.1 192.168.1.5 192.168.1.9 192.168.1.10 192.168.1.20 \
192.168.2.5 192.168.2.9 192.168.4.0 192.168.5.5 \
100.64.0.0 100.64.0.1 100.64.0.9 100.64.1.1 100.100.100.200 100.100.100.201 \
169.254.0.0 169.254.1.1 169.254.169.254 169.254.170.2 \
1.2.3.4 1.2.3.04 11.0.0.0 11.2.3.4 8.0.0.0 "
# Each whole run of digits and dots is taken (so a dot or a comma next to an
# address cannot hide it, and two addresses in one line are both read); a
# run with exactly four parts of 1 to 3 digits, after its edge dots are
# dropped, is an address.
ips="$(tgrep -noE -e '[0-9.]*[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}[0-9.]*' |
	grep -aEv '^(web/package-lock\.json|server/go\.sum|scripts/audit-public\.sh):' |
	awk -F: '{ip=$0; sub(/^[^:]*:[^:]*:/,"",ip); gsub(/^\.+|\.+$/,"",ip); print $1":"$2":"ip}' |
	awk -F: -v ok="$ip_examples" '{
		n=split($3,o,"."); if (n!=4) next;
		bad=0; for(i=1;i<=4;i++) if (o[i]+0>255 || o[i]=="" || length(o[i])>3) bad=1; if (bad) next;
		a=o[1]+0; b=o[2]+0; c=o[3]+0; d=o[4]+0;
		if (a==127 || a==0 || a==255 || a>=224) next;
		if (a==192 && b==0 && c==2) next;
		if (a==198 && b==51 && c==100) next;
		if (a==203 && b==0 && c==113) next;
		if (a==198 && (b==18 || b==19)) next;
		if (a==192 && b==0 && c==0) next;
		if (a==192 && b==88 && c==99) next;
		if ($3=="93.184.216.34" || $3=="8.8.8.8" || $3=="1.1.1.1") next;
		if ($3 ~ /^168\.63\.129\./ || $3=="128.0.0.0" || $3=="192.0.0.0" || $3=="192.88.99.0") next;
		special = (a==10) || (a==172 && b>=16 && b<=31) || (a==192 && b==168) ||
			(a==100 && b>=64 && b<=127) || (a==169 && b==254) || (a<=30 && b<=30 && c<=30 && d<=30);
		if (special && index(ok, " " $3 " ") > 0) next;
		print
	}' | sort -u || true)"
if [[ -n "$ips" ]]; then
	echo "== HIT: IPv4 addresses to review"
	printf '%s\n' "$ips" | cut -c1-200
	hits=$((hits + 1))
fi

# Global (2000::/3) and unique local (fc00::/7) IPv6 addresses. The
# documentation prefix 2001:db8::/32 passes; so do these exact values the
# network guard tests use (public DNS resolvers, 6to4 and Teredo forms of
# example IPv4 addresses, bare prefixes, example unique local addresses).
ip6_examples=" fc00:: fc00::1 fd00:: fd00::1 fd00:ec2:: fd00:ec2::254 fd00:dead:: fd00:dead::5 fd00:dead::99 fd12:: fd12::1 fd12::7 \
2001:: 2002:: 2002:7f00:1:: 2002:808:808::1 2002:a00:1::1 2002:a9fe:a9fe::1 2002:c0a8:105::1 \
2001:0:7f00:1::1 2001:0:808:808:0:0:80ff:fffe 2001:0:c0a8:105::1 \
2606:4700:4700::1111 2606:4700::1111 2001:4860:4860::8888 2606:2800:220:1::1 "
ip6="$(tgrep -noE -e '(^|[^0-9A-Za-z:.])([23][0-9A-Fa-f]{3}|[fF][cCdD][0-9A-Fa-f]{2})(:[0-9A-Fa-f]{0,4}){2,7}' |
	grep -aEv '^(web/package-lock\.json|server/go\.sum|scripts/audit-public\.sh):' |
	awk -F: -v ok="$ip6_examples" '{
		f=$1; l=$2; ip=$0; sub(/^[^:]*:[^:]*:/,"",ip); sub(/^[^0-9A-Fa-f]/,"",ip); v=tolower(ip);
		if (v ~ /^2001:0?db8:/) next;
		if (index(ok, " " v " ") > 0) next;
		print f ":" l ":" ip
	}' | sort -u || true)"
if [[ -n "$ip6" ]]; then
	echo "== HIT: IPv6 addresses to review"
	printf '%s\n' "$ip6" | cut -c1-200
	hits=$((hits + 1))
fi

# Files that must not ship, at any depth, in any letter case. The Claude Code
# plugin's own .mcp.json is the one allowed MCP config: it may hold only
# ${...} placeholders, never a URL, a host or a token.
noship="$(find . -path ./.git -prune -o \( -iname CLAUDE.local.md -o -iname .gitlab-ci.yml -o -iname .mcp.json -o -iname .npmrc -o -iname '.yarnrc*' \) -print 2>/dev/null |
	grep -avxF './plugins/launchpad/.mcp.json' || true)"
if [[ -n "$noship" ]]; then
	echo "== HIT: files that must not ship"
	printf '%s\n' "$noship"
	hits=$((hits + 1))
fi
# The plugin's .mcp.json, parsed as JSON: every string is a bare ${NAME}
# placeholder (no ${NAME:-default}), except "command", which is a path under
# ${CLAUDE_PLUGIN_ROOT}, and "type", which may be "stdio". No literal
# argument, URL, host or token; key names are plain names.
if [[ -e plugins/launchpad/.mcp.json ]]; then
	mcpbad="$(perl -MJSON::PP -e '
		open(my $h, "<:raw", $ARGV[0]) or die "cannot read $ARGV[0]\n";
		local $/; my $raw = <$h>; close $h;
		my $j = eval { JSON::PP->new->decode($raw) };
		unless (defined $j) { print "it is not valid JSON\n"; exit 0 }
		my @bad;
		my $walk; $walk = sub {
			my ($v, $key) = @_;
			if (ref $v eq "HASH") {
				for my $k (sort keys %$v) {
					push @bad, "a key is not a plain name" unless $k =~ /^[A-Za-z_][A-Za-z0-9_.-]*$/;
					$walk->($v->{$k}, $k);
				}
				return;
			}
			if (ref $v eq "ARRAY") { $walk->($_, $key) for @$v; return }
			return if !defined $v || JSON::PP::is_bool($v);
			my $ok = $v =~ /^\$\{[A-Za-z_][A-Za-z0-9_.]*\}$/
				|| ($key eq "command" && $v =~ m{^\$\{CLAUDE_PLUGIN_ROOT\}/[A-Za-z0-9_./-]+$} && $v !~ /\.\./)
				|| ($key eq "type" && $v eq "stdio");
			push @bad, "the value of \"$key\" is not a placeholder" unless $ok;
		};
		$walk->($j, "");
		print "$_\n" for @bad;
	' plugins/launchpad/.mcp.json)" || fail2 "the check of plugins/launchpad/.mcp.json could not run"
	if [[ -n "$mcpbad" ]]; then
		echo "== HIT: plugins/launchpad/.mcp.json"
		printf '%s\n' "$mcpbad"
		hits=$((hits + 1))
	fi
fi
stray="$(find . -path ./.git -prune -o -path ./node_modules -prune -o \( -iname '.env*' -o -iname '*.env' -o -iname '*.pem' -o -iname '*.key' -o -iname 'id_rsa*' -o -iname 'id_ed25519*' -o -iname '.DS_Store' \) ! -iname '.env.example' ! -iname '*.env.example' -type f -print 2>/dev/null || true)"
if [[ -n "$stray" ]]; then
	echo "== HIT: secret-looking or stray files"
	printf '%s\n' "$stray"
	hits=$((hits + 1))
fi

# Paths that must not ship anywhere in the tree, in any letter case:
# installed packages and local agent settings.
forbidden="$(find . -path ./.git -prune -o \( -iname node_modules -o -iname .claude \) -prune -print 2>/dev/null || true)"
if [[ -n "$forbidden" ]]; then
	echo "== HIT: paths that must not ship"
	printf '%s\n' "$forbidden"
	hits=$((hits + 1))
fi

# An .env.example holds placeholders only: a key whose name says secret
# (PASSWORD, SECRET, TOKEN, PEPPER, KEY, CREDENTIAL, or a PAT or API part)
# must be empty, all zeros, or say "placeholder". The value is never printed.
envbad=""
while IFS= read -r f; do
	[[ -n "$f" ]] || continue
	envbad+="$(awk -v f="${f#./}" '
		/^[[:space:]]*(#|$)/ { next }
		{
			line = $0; sub(/^[[:space:]]*export[[:space:]]+/, "", line)
			i = index(line, "="); if (i == 0) next
			k = substr(line, 1, i - 1); v = substr(line, i + 1)
			gsub(/^[[:space:]]+|[[:space:]]+$/, "", k); gsub(/^[[:space:]]+|[[:space:]]+$/, "", v)
			if (v ~ /^".*"$/ || v ~ /^\047.*\047$/) v = substr(v, 2, length(v) - 2)
			K = toupper(k)
			if (K !~ /(PASSWORD|SECRET|TOKEN|PEPPER|KEY|CREDENTIAL)/ && K !~ /(^|_)(PAT|API)(_|$)/) next
			if (v == "" || v ~ /^0+$/ || tolower(v) ~ /placeholder/) next
			print f ":" NR ": " k " has a value that is not a placeholder"
		}' "$f")"$'\n'
done < <(find . -path ./.git -prune -o -type f \( -name '.env.example' -o -name '*.env.example' \) -print 2>/dev/null)
envbad="$(printf '%s' "$envbad" | grep -v '^$' || true)"
if [[ -n "$envbad" ]]; then
	echo "== HIT: .env.example values that are not placeholders"
	printf '%s\n' "$envbad"
	hits=$((hits + 1))
fi

# Symbolic links and git submodules never ship: a link can point outside the
# tree, and a submodule pulls in another repository.
linked="$(find . -path ./.git -prune -o \( -type l -o -iname .gitmodules \) -print 2>/dev/null || true)"
linked+="$(find . -mindepth 2 -iname .git -print 2>/dev/null | sed 's/^/\n/' || true)"
linked="$(printf '%s\n' "$linked" | grep -av '^$' || true)"
if [[ -n "$linked" ]]; then
	echo "== HIT: symbolic links or git submodules"
	printf '%s\n' "$linked"
	hits=$((hits + 1))
fi

# Binary files: perl's -B test (the same on every system; grep -I differs
# between GNU grep, BSD grep and ugrep), or a NUL byte anywhere in the file.
# Only the self-hosted fonts and the README screenshots are allowed (by path,
# one folder each, no subfolders); any other binary file is a hit.
files="$(find . -path ./.git -prune -o -name node_modules -prune -o -type f -print 2>"$work/find2.err")" ||
	fail2 "find failed: $(head -3 "$work/find2.err")"
binaries="$(perl -ne 'chomp; next unless length; my $f = $_; next unless -s $f;
	open(my $h, "<:raw", $f) or die "cannot read $f\n"; local $/; my $d = <$h>; close $h;
	print "$f\n" if -B $f || index($d, "\0") >= 0' <<<"$files")" || fail2 "perl failed while looking for binary files"
binaries="$(awk '{ sub(/^\.\//, ""); print }' <<<"$binaries" | sort)"
binhits="$(printf '%s\n' "$binaries" | grep -av '^$' | grep -aEv "$allowed_bin_path" || true)"
echo "== binary files: $(printf '%s\n' "$binaries" | grep -avc '^$' || true) (allowed: web/src/assets/fonts/*.woff2, docs/screenshots/*.png)"
if [[ -n "$binhits" ]]; then
	echo "== HIT: binary files outside the allow list"
	printf '%s\n' "$binhits"
	hits=$((hits + 1))
fi

# The allowed binary paths must hold what their name says, and nothing else,
# checked for every file on those paths, binary or not:
# - no host, name or token pattern in the raw bytes;
# - a .woff2 starts with wOF2 and its header length equals the file length;
# - a .png has the PNG signature, IHDR first, only the chunks below (no text,
#   no Exif: tEXt, zTXt, iTXt and eXIf are refused), the fixed chunk lengths
#   (IHDR 13, gAMA 4, sRGB 1, pHYs 9, IEND 0; PLTE 3 to 768 in steps of 3;
#   tRNS at most 256), ends with IEND with no byte after it, and its image
#   data inflates and holds no host, name or token pattern either.
blob_re="$ident_re|ghp_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|glpat-[A-Za-z0-9_-]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|odt_[A-Za-z0-9]{43}|AIza[0-9A-Za-z_-]{35}|sk_live_[0-9A-Za-z]+|hooks\.slack\.com/services/T[A-Z0-9]{8}|PRIVATE KEY-----"
allowed_files="$(awk '{ sub(/^\.\//, ""); print }' <<<"$files" | grep -aE "$allowed_bin_path" || true)"
structbad="$(BLOB_RE="$blob_re" perl -MCompress::Zlib -ne '
	chomp; next unless length; my $f = $_;
	open(my $h, "<:raw", $f) or die "cannot read $f\n";
	local $/; my $d = <$h>; close $h; $d = "" unless defined $d;
	my $re = qr/$ENV{BLOB_RE}/i;
	if ($d =~ $re) { print "$f: the raw bytes match a host, name or token pattern\n"; next }
	if ($f =~ /\.woff2$/) {
		if (substr($d, 0, 4) ne "wOF2") { print "$f: does not start with wOF2\n"; next }
		if (length($d) < 12) { print "$f: too short for a WOFF2 header\n"; next }
		my $len = unpack("N", substr($d, 8, 4));
		print "$f: the WOFF2 header says $len bytes, the file has " . length($d) . "\n" if $len != length($d);
		next;
	}
	if (substr($d, 0, 8) ne "\x89PNG\r\n\x1a\n") { print "$f: not a PNG file\n"; next }
	my %ok = map { $_ => 1 } qw(IHDR PLTE tRNS gAMA sRGB pHYs IDAT IEND);
	my %fixed = (IHDR => 13, gAMA => 4, sRGB => 1, pHYs => 9, IEND => 0);
	my ($i, $end, $first, $idat) = (8, 0, 1, "");
	while ($i + 12 <= length $d) {
		my ($n, $t) = unpack("N a4", substr($d, $i, 8));
		(my $s = $t) =~ s/[^ -~]/?/g;
		if ($first && $t ne "IHDR") { print "$f: the first chunk is not IHDR\n"; $end = -1; last }
		$first = 0;
		unless ($ok{$t}) { print "$f: chunk $s is not allowed\n"; $end = -1; last }
		if ($i + 12 + $n > length $d) { print "$f: a chunk runs past the end\n"; $end = -1; last }
		if (exists $fixed{$t} && $n != $fixed{$t}) { print "$f: chunk $s has length $n, not $fixed{$t}\n"; $end = -1; last }
		if ($t eq "PLTE" && ($n == 0 || $n % 3 || $n > 768)) { print "$f: chunk PLTE has length $n\n"; $end = -1; last }
		if ($t eq "tRNS" && $n > 256) { print "$f: chunk tRNS has length $n\n"; $end = -1; last }
		$idat .= substr($d, $i + 8, $n) if $t eq "IDAT";
		$i += 12 + $n;
		if ($t eq "IEND") { $end = 1; last }
	}
	next if $end < 0;
	if ($end == 0) { print "$f: no IEND chunk\n"; next }
	if ($i != length $d) { print "$f: " . (length($d) - $i) . " byte(s) after IEND\n"; next }
	my $px = uncompress($idat);
	unless (defined $px) { print "$f: the image data does not inflate\n"; next }
	print "$f: the image data matches a host, name or token pattern\n" if $px =~ $re;
' <<<"$allowed_files")" || fail2 "perl failed in the structure check of the allowed binary files"
if [[ -n "$structbad" ]]; then
	echo "== HIT: allowed binary files with a wrong structure"
	printf '%s\n' "$structbad"
	hits=$((hits + 1))
fi

identity_check

if ((run_gitleaks)); then
	command -v docker >/dev/null 2>&1 || {
		echo "audit-public.sh: --gitleaks needs docker" >&2
		exit 2
	}
	echo "== gitleaks ($GITLEAKS_IMAGE, --no-git)"
	if ! docker run --rm -v "$PWD:/repo:ro" "$GITLEAKS_IMAGE" detect --no-git --source /repo --redact --no-banner -v; then
		echo "== HIT: gitleaks reported findings"
		hits=$((hits + 1))
	fi
fi

end_if_failed
if ((hits > 0)); then
	echo "audit-public.sh: $hits check(s) with hits"
	exit 1
fi
echo "audit-public.sh: no hits"
