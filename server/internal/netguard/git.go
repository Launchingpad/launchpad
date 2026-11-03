package netguard

import (
	"fmt"
	"net/netip"
	"strings"
)

// GitServiceNames are the compose service names and Docker DNS names the git
// policy refuses before resolution (T17 #31 rule 3).
var GitServiceNames = []string{
	"postgres", "app", "silo", "backup", "web",
	"host.docker.internal", "gateway.docker.internal",
}

// GitPolicy is the policy of the git API clients (T17 #31).
//
// providers is GIT_ALLOW_PRIVATE_PROVIDERS and cidrs is GIT_ALLOW_PRIVATE_CIDRS.
// One rule: the opened private ranges are the union of the five standard
// ranges (when providers is true; a built-in set, NOT run through the CIDR
// parser) and the listed CIDRs (validated by ParseCIDRs). With both unset no
// private range opens. The always-refused lists, the metadata addresses and
// the app's own subnets and gateway (dyn) are removed last. The git policy has
// no gateway exception.
func GitPolicy(providers bool, cidrs string, dyn Dynamic) (Policy, error) {
	listed, err := ParseCIDRs(cidrs)
	if err != nil {
		return Policy{}, fmt.Errorf("GIT_ALLOW_PRIVATE_CIDRS: %w", err)
	}
	var private []netip.Prefix
	if providers {
		private = append(private, StandardPrivate...)
	}
	private = append(private, listed...)
	return Policy{
		Private: private,
		Dynamic: dyn,
		Names:   append([]string(nil), GitServiceNames...),
	}, nil
}

// ParseSwitch reads a boolean environment switch: "1" or "true" is on, empty,
// "0" or "false" is off, anything else is an error (a typo must not silently
// leave the switch off).
func ParseSwitch(name, v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true":
		return true, nil
	case "", "0", "false":
		return false, nil
	}
	return false, fmt.Errorf("%s must be 1 or 0", name)
}

// GitPolicyFromEnv reads GIT_ALLOW_PRIVATE_PROVIDERS and
// GIT_ALLOW_PRIVATE_CIDRS through getenv and builds the git policy.
func GitPolicyFromEnv(getenv func(string) string, dyn Dynamic) (Policy, error) {
	on, err := ParseSwitch("GIT_ALLOW_PRIVATE_PROVIDERS", getenv("GIT_ALLOW_PRIVATE_PROVIDERS"))
	if err != nil {
		return Policy{}, err
	}
	return GitPolicy(on, getenv("GIT_ALLOW_PRIVATE_CIDRS"), dyn)
}
