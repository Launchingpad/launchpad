// Package clients builds the provider API client of one integration (T17 P1,
// LAUN-7). It is the one place that knows every provider client, so the
// tracker can take a plain function.
package clients

import (
	"fmt"

	"devops/internal/gitapi"
	"devops/internal/gitapi/bitbucket"
	"devops/internal/gitapi/github"
	"devops/internal/gitapi/gitlab"
	"devops/internal/scm"
)

// Factory returns the client constructor for the registered providers (gitlab,
// github and bitbucket, Cloud and Data Center). o carries the guarded
// HTTP client (gitapi.NewHTTPClient), so no client can dial around netguard.
func Factory(o gitapi.Options) func(provider, apiRoot, token string) (scm.APIClient, error) {
	return func(provider, apiRoot, token string) (scm.APIClient, error) {
		switch provider {
		case "gitlab":
			return gitlab.NewAt(apiRoot, token, o)
		case "github":
			return github.NewAt(apiRoot, token, o)
		case "bitbucket":
			return bitbucket.NewAt(apiRoot, token, o)
		}
		return nil, fmt.Errorf("no API client for provider %q", provider)
	}
}
