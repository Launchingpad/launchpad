package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Attachments (decisions-T2 #28-#30). The REST flow is decisions-T3b: prepare
// a presigned PUT, the CLIENT puts the bytes, then create the attachment from
// the upload key. The MCP server never reads a local file: like Linear's MCP
// tools it hands the presigned request to the agent, which PUTs the file.

// linkArg is one save_issue `links` item, Linear's {url, title}.
type linkArg struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

func decodeLinks(raw json.RawMessage) ([]linkArg, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var in []linkArg
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("links must be an array of {url, title}: %w", err)
	}
	out := make([]linkArg, 0, len(in))
	for _, l := range in {
		l.URL = strings.TrimSpace(l.URL)
		if l.URL == "" {
			return nil, fmt.Errorf("a link has no url")
		}
		dup := false
		for _, o := range out {
			if o.URL == l.URL {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, l)
		}
	}
	return out, nil
}

// syncLinks adds save_issue's links as link attachments. It is add-only: a
// URL already attached to the issue (as a link) is skipped, and so is a 409
// from the API, so repeating a call writes nothing. It reports whether it
// created anything.
func (t *Tools) syncLinks(ctx context.Context, ref string, a args) (bool, error) {
	if !a.set("links") {
		return false, nil
	}
	links, err := decodeLinks(a.raw("links"))
	if err != nil || len(links) == 0 {
		return false, err
	}
	p, err := orgPath("issues", ref, "attachments")
	if err != nil {
		return false, err
	}
	var have Page[struct {
		Kind string  `json:"kind"`
		URL  *string `json:"url"`
	}]
	if err := t.c.Get(ctx, p, nil, &have); err != nil {
		return false, err
	}
	present := map[string]bool{}
	for _, at := range have.Items {
		if at.Kind == "link" && at.URL != nil {
			present[*at.URL] = true
		}
	}
	added := false
	for _, l := range links {
		if present[l.URL] {
			continue
		}
		body := map[string]string{"kind": "link", "url": l.URL}
		if l.Title != "" {
			body["title"] = l.Title
		}
		err := t.c.Post(ctx, p, body, nil)
		switch {
		case err == nil:
			added = true
			present[l.URL] = true
		case IsStatus(err, 409):
			// Already there (a concurrent call): add-only means done.
		default:
			return added, fmt.Errorf("link %s: %w", l.URL, err)
		}
	}
	return added, nil
}

// prepareAttachmentUpload is Linear's prepare_attachment_upload on
// POST /attachments/prepare. The result keeps every REST field and adds
// Linear's names: uploadRequest {url, method, headers} and assetUrl (the
// upload key, which create_attachment_from_upload takes back).
func prepareAttachmentUpload(ctx context.Context, t *Tools, a args) (any, error) {
	// Check the issue first, so a wrong id fails before any upload.
	ip, err := orgPath("issues", a.str("issue"))
	if err != nil {
		return nil, err
	}
	var is issueHead
	if err := t.c.Get(ctx, ip, nil, &is); err != nil {
		return nil, err
	}
	var up map[string]any
	err = t.c.Post(ctx, "attachments/prepare", map[string]any{
		"filename": a.str("filename"), "contentType": a.str("contentType"), "sizeBytes": a.integer("size"),
	}, &up)
	if err != nil {
		return nil, err
	}
	key, _ := up["uploadKey"].(string)
	if key == "" {
		return nil, fmt.Errorf("prepare upload: unexpected response")
	}
	up["assetUrl"] = key
	up["issue"] = is.Identifier
	up["uploadRequest"] = map[string]any{"url": up["uploadUrl"], "method": up["method"], "headers": up["headers"]}
	return up, nil
}

// createAttachmentFromUpload is Linear's create_attachment_from_upload on
// POST /issues/{issue}/attachments with kind file.
func createAttachmentFromUpload(ctx context.Context, t *Tools, a args) (any, error) {
	p, err := orgPath("issues", a.str("issue"), "attachments")
	if err != nil {
		return nil, err
	}
	body := map[string]string{"kind": "file", "uploadKey": a.str("assetUrl")}
	if s := a.str("title"); s != "" {
		body["title"] = s
	}
	if s := a.str("subtitle"); s != "" {
		body["subtitle"] = s
	}
	var out json.RawMessage
	err = t.c.Post(ctx, p, body, &out)
	if err != nil && strings.Contains(a.str("assetUrl"), "uploads.linear.app") {
		return nil, fmt.Errorf("%w (a Linear assetUrl cannot be attached here: run prepare_attachment_upload on this server first)", err)
	}
	return out, err
}

// errLinearCycleID is the answer for a cycle UUID this tracker does not have,
// which today is nearly always a Linear cycle id (decisions-T2 #27).
func errLinearCycleID(ref string) error {
	return fmt.Errorf("no cycle %s in this tracker. A Linear cycle id is not a cycle id here: "+
		"pass the cycle number or `active`", ref)
}
