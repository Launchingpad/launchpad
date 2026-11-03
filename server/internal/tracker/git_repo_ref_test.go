package tracker

import "testing"

// A stored repo becomes the ref a client call takes: Data Center needs the
// opaque "KEY/slug#id" (its REST paths take the project key and slug).
func TestGitRepoRef(t *testing.T) {
	const cloud = "{11111111-2222-3333-4444-555555555555}/{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}"
	tests := []struct{ provider, key, path, id string }{
		{"github", "4242", "acme/repo", "4242"},
		{"gitlab", "42", "group/project", "42"},
		{"bitbucket", cloud, "ws/repo", cloud},
		{"bitbucket", "4242", "PROJ/example-app", "PROJ/example-app#4242"},
		{"bitbucket", "4242", "~USER/my.repo", "~USER/my.repo#4242"},
		{"bitbucket", "4242", "a label with spaces", "4242"}, // not a KEY/slug label: the client refuses it
	}
	for _, tt := range tests {
		ref := gitRepoRef(tt.provider, tt.key, tt.path)
		if ref.ID != tt.id || ref.Key != tt.key || ref.Path != tt.path {
			t.Errorf("%s %s %q -> %+v, want id %q", tt.provider, tt.key, tt.path, ref, tt.id)
		}
	}
}
