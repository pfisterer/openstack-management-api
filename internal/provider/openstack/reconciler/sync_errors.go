package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/gophercloud/gophercloud"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
)

// What a pass could not carry out for a project is written onto its node (model.Binding.SyncErrors), so the portal shows it at the resource instead of the resource reading as granted. Incident 19.10.2026: a network availability failed with OverQuota on every pass for days while the project's page said "not in use".

// maxSyncDetail bounds the provider's message stored on the node.
const maxSyncDetail = 500

// syncErrorFor turns an error from OpenStack into what the node keeps.
func syncErrorFor(kind string, err error) model.SyncError {
	return model.SyncError{Kind: kind, Reason: syncReason(err), Detail: syncDetail(err)}
}

// syncReason classifies an error from OpenStack into one of the model.Sync* reasons.
func syncReason(err error) string {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "overquota") || strings.Contains(msg, "quota exceeded") || strings.Contains(msg, "quotaexceeded") {
		return model.SyncOverQuota
	}
	var sce gophercloud.StatusCodeError
	if errors.As(err, &sce) {
		switch code := sce.GetStatusCode(); {
		case code == 401 || code == 403:
			return model.SyncForbidden
		case code == 404:
			return model.SyncNotFound
		case code == 409:
			return model.SyncConflict
		case code == 408 || code == 429 || code >= 500:
			return model.SyncUnreachable
		}
		return model.SyncFailed
	}
	var netErr net.Error
	if errors.As(err, &netErr) || strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") {
		return model.SyncUnreachable
	}
	return model.SyncFailed
}

// syncDetail is the error as an admin wants to read it: what was attempted and the service's own message, without the request line and the JSON around it. Gophercloud's message is "Expected HTTP response code [201] when accessing [POST https://…], but got 409 instead" followed by the body; the body's "message" (Neutron, Nova, Cinder all have one) replaces all of that.
func syncDetail(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "Expected HTTP response code"); i >= 0 {
		prefix := msg[:i]
		rest := msg[i:]
		code := ""
		var sce gophercloud.StatusCodeError
		if errors.As(err, &sce) {
			code = "HTTP " + strconv.Itoa(sce.GetStatusCode())
		}
		if text := bodyMessage(rest); text != "" {
			msg = prefix + strings.TrimSpace(strings.Join([]string{code, text}, " "))
		} else if code != "" {
			msg = prefix + code
		}
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > maxSyncDetail {
		msg = msg[:maxSyncDetail] + "…"
	}
	return msg
}

// bodyMessage finds the JSON body in a gophercloud error message and returns its type and message ("OverQuota: Quota exceeded for resources: ['rbac_policy']."); "" when there is none.
func bodyMessage(s string) string {
	i := strings.Index(s, "{")
	if i < 0 {
		return ""
	}
	var body map[string]any
	if json.Unmarshal([]byte(s[i:]), &body) != nil {
		return ""
	}
	// {"NeutronError": {"type": …, "message": …}}, {"forbidden": {"message": …}}, or flat.
	for _, v := range append([]any{body}, mapValues(body)...) {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		text, _ := m["message"].(string)
		if text == "" {
			continue
		}
		if typ, _ := m["type"].(string); typ != "" {
			return typ + ": " + text
		}
		return text
	}
	return ""
}

func mapValues(m map[string]any) []any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]any, 0, len(m))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

// grantResourceIDs are the availabilities of the catalogue that syncGrants carries out.
func grantResourceIDs(defs []osclient.ManagedResource) []string {
	var out []string
	for _, def := range defs {
		if def.IsBool() && def.Grant != nil {
			out = append(out, def.ID)
		}
	}
	return out
}

// quotaResourceIDs are the resources of the catalogue that a quota update sets.
func quotaResourceIDs(defs []osclient.ManagedResource) []string {
	var out []string
	for _, def := range defs {
		if def.OSQuotaField != "" && !def.IsBool() {
			out = append(out, def.ID)
		}
	}
	return out
}

// quotaSyncErrors pins a failed quota update on the resources it concerns: those of the service that refused, or all quota resources when it is not known which.
func quotaSyncErrors(defs []osclient.ManagedResource, err error) map[string]model.SyncError {
	e := syncErrorFor(model.SyncKindQuota, err)
	var qe *osclient.QuotaUpdateError
	var fields []string
	if errors.As(err, &qe) {
		fields = osclient.QuotaServiceFields[qe.Service]
	}
	out := map[string]model.SyncError{}
	for _, def := range defs {
		if def.OSQuotaField == "" || def.IsBool() {
			continue
		}
		if fields != nil && !slices.Contains(fields, def.OSQuotaField) {
			continue
		}
		out[def.ID] = e
	}
	return out
}

// recordSyncErrors writes what this pass found for a leaf; nothing in a dry run, which attempted nothing. An error kept for a resource the catalogue no longer has is cleared too: nothing will ever retry it.
func (r *Reconciler) recordSyncErrors(ctx context.Context, leaf model.Node, attempted []string, found map[string]model.SyncError) {
	if r.cfg.DryRun {
		return
	}
	known := map[string]bool{}
	for _, def := range r.resources() {
		known[def.ID] = true
	}
	for id := range leaf.Binding(osclient.Provider).SyncErrors {
		if !known[id] && !slices.Contains(attempted, id) {
			attempted = append(attempted, id)
		}
	}
	if len(attempted) == 0 {
		return
	}
	if err := r.nodes.RecordSyncErrors(ctx, leaf.ID, attempted, found); err != nil {
		r.log.Warnw("Failed to record which resources could not be carried out", "node_id", leaf.ID, "error", err)
	}
}
