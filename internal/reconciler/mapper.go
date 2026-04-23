package reconciler

import (
	"github.com/pfisterer/openstack-management-api/internal/common"
	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
)

// quotaField pairs a getter and setter for a named field on osclient.QuotaSet.
// This lets the mapper drive field access from ManagedProject.OSQuotaField strings
// without reflection.
type quotaField struct {
	get func(osclient.QuotaSet) int
	set func(*osclient.QuotaSet, int)
}

// quotaFields maps every OSQuotaField / OSLinkedField value that ManagedResources may
// reference to the corresponding QuotaSet accessor. Add a new entry here whenever a new
// OS-level quota field needs to be exposed.
var quotaFields = map[string]quotaField{
	// Compute
	"cores":     {func(q osclient.QuotaSet) int { return q.Cores }, func(q *osclient.QuotaSet, v int) { q.Cores = v }},
	"ram":       {func(q osclient.QuotaSet) int { return q.RAM }, func(q *osclient.QuotaSet, v int) { q.RAM = v }},
	"instances": {func(q osclient.QuotaSet) int { return q.Instances }, func(q *osclient.QuotaSet, v int) { q.Instances = v }},
	// Block storage
	"gigabytes": {func(q osclient.QuotaSet) int { return q.Gigabytes }, func(q *osclient.QuotaSet, v int) { q.Gigabytes = v }},
	"volumes":   {func(q osclient.QuotaSet) int { return q.Volumes }, func(q *osclient.QuotaSet, v int) { q.Volumes = v }},
	"snapshots": {func(q osclient.QuotaSet) int { return q.Snapshots }, func(q *osclient.QuotaSet, v int) { q.Snapshots = v }},
	// Network
	"networks":        {func(q osclient.QuotaSet) int { return q.Networks }, func(q *osclient.QuotaSet, v int) { q.Networks = v }},
	"subnets":         {func(q osclient.QuotaSet) int { return q.Subnets }, func(q *osclient.QuotaSet, v int) { q.Subnets = v }},
	"ports":           {func(q osclient.QuotaSet) int { return q.Ports }, func(q *osclient.QuotaSet, v int) { q.Ports = v }},
	"routers":         {func(q osclient.QuotaSet) int { return q.Routers }, func(q *osclient.QuotaSet, v int) { q.Routers = v }},
	"floating_ips":    {func(q osclient.QuotaSet) int { return q.FloatingIPs }, func(q *osclient.QuotaSet, v int) { q.FloatingIPs = v }},
	"security_groups": {func(q osclient.QuotaSet) int { return q.SecurityGroups }, func(q *osclient.QuotaSet, v int) { q.SecurityGroups = v }},
}

// multiplierOf returns the effective OS unit multiplier for a resource.
// A stored OSMultiplier of 0 is treated as 1 (identity).
func multiplierOf(r common.ManagedProject) int {
	if r.OSMultiplier == 0 {
		return 1
	}
	return r.OSMultiplier
}

// StaticProjectQuotaDefaults builds a QuotaSet from every ManagedProject that has
// Static: true, using each resource's Default value. This replaces the old
// ReconcilerConfiguration.DefaultNetworkQuotas struct: to change a static quota
// default, update the Default field in the ManagedProject definition; no other
// file needs to change.
func StaticProjectQuotaDefaults(resources []common.ManagedProject) osclient.QuotaSet {
	var qs osclient.QuotaSet
	for _, res := range resources {
		if !res.Static || res.OSQuotaField == "" {
			continue
		}
		fa, ok := quotaFields[res.OSQuotaField]
		if !ok {
			continue
		}
		converted := res.Default * multiplierOf(res)
		fa.set(&qs, converted)
	}
	return qs
}

// mergeStaticIntoQuotaSet writes every field from src into dst where src has a
// non-zero value. Used to overlay static quota defaults on top of a managed QuotaSet.
func mergeStaticIntoQuotaSet(dst *osclient.QuotaSet, src osclient.QuotaSet) {
	for _, fa := range quotaFields {
		if v := fa.get(src); v != 0 {
			fa.set(dst, v)
		}
	}
}

// ProjectQuotaToQuotaSet translates a ProjectQuota (domain model) into an OpenStack
// QuotaSet, driven entirely by the ManagedProject definitions.
//
// For each resource that has an OSQuotaField the domain value is multiplied by
// OSMultiplier (default 1) before being written. If OSLinkedField is set the same
// converted value is written there too (used to derive Instances from Cores).
//
// Resources without an OSQuotaField (e.g. GPU without custom quota support) are
// silently skipped.
func ProjectQuotaToQuotaSet(resources []common.ManagedProject, rq common.ProjectQuota) osclient.QuotaSet {
	var qs osclient.QuotaSet
	for _, res := range resources {
		if res.OSQuotaField == "" {
			continue
		}
		fa, ok := quotaFields[res.OSQuotaField]
		if !ok {
			continue
		}
		converted := rq[res.ID] * multiplierOf(res)
		fa.set(&qs, converted)

		if res.OSLinkedField != "" {
			if lfa, ok := quotaFields[res.OSLinkedField]; ok {
				lfa.set(&qs, converted)
			}
		}
	}
	return qs
}

// QuotaSetToProjectQuota translates an OpenStack QuotaSet back to the domain model,
// driven by the ManagedProject definitions.
//
// Each resource's OS value is divided by OSMultiplier (default 1) to recover the
// domain unit (e.g. MB → GB for RAM).
func QuotaSetToProjectQuota(resources []common.ManagedProject, qs osclient.QuotaSet) common.ProjectQuota {
	rq := make(common.ProjectQuota, len(resources))
	for _, res := range resources {
		if res.OSQuotaField == "" {
			continue
		}
		fa, ok := quotaFields[res.OSQuotaField]
		if !ok {
			continue
		}
		rq[res.ID] = fa.get(qs) / multiplierOf(res)
	}
	return rq
}

// IsProjectOvercommitted returns true when any resource marked OSOvercommitCheck has its
// in-use value (from the OS quota detail) exceeding the approved quota limit.
// A limit of common.UnlimitedQuota (-1) or zero is never treated as overcommitted.
func IsProjectOvercommitted(resources []common.ManagedProject, approvedRQ common.ProjectQuota, detail *osclient.ProjectQuotaDetail) bool {
	if detail == nil {
		return false
	}
	for _, res := range resources {
		if !res.OSOvercommitCheck || res.OSQuotaField == "" {
			continue
		}
		fa, ok := quotaFields[res.OSQuotaField]
		if !ok {
			continue
		}
		inUse := fa.get(detail.InUse) / multiplierOf(res)
		limit := approvedRQ[res.ID]
		if limit == common.UnlimitedQuota || limit <= 0 {
			continue
		}
		if inUse > limit {
			return true
		}
	}
	return false
}
