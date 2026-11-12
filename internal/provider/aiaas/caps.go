package aiaas

import (
	"github.com/pfisterer/cloud-self-service-golib/msg"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// The caps of the AI service (common.Cap, decided 10.11.2026, docs/plan-caps-katalog.md): upper bounds a budget sets for the projects below it, handed to the AI service as the strictest value along each project's chain of budgets (ReleaseFor, ScratchFor, BatchPriorityFor, BatchJobsFor). Before, they were four fields of the tree's nodes.
//
// Release times are when a workspace or a GPU kernel goes back, per kind of use: CPU (CPU workspaces and the notebook of "CPU + GPU kernel") and GPU (GPU workspaces and GPU kernels). Counted is idle time, with and without an open tab; the upper bound ends a workspace whatever it does, 0 = none, only for GPU (and at the AI service only for projects with GPUs of their own). A time no budget sets is left to the AI service's default. The local storage per workspace is what a workspace or GPU kernel may write outside its home. The batch priority says whether batch jobs may be preempted; none set = preemptible. Batch jobs are GPU jobs without a workspace: a budget has them only where it sets them (Grant), as many per person at the same time and as long per job as it allows.

// The ids of the caps.
const (
	CapReleaseCPUIdleOpen   = "release-cpu-idle-open"
	CapReleaseCPUIdleClosed = "release-cpu-idle-closed"
	CapReleaseCPUMaxHours   = "release-cpu-max-hours"
	CapReleaseGPUIdleOpen   = "release-gpu-idle-open"
	CapReleaseGPUIdleClosed = "release-gpu-idle-closed"
	CapReleaseGPUMaxHours   = "release-gpu-max-hours"
	CapWorkspaceScratchGB   = "workspace-scratch-gb"
	CapBatchPriority        = "batch-priority"
	CapBatchJobsPerPerson   = "batch-jobs-per-person"
	CapBatchMaxHours        = "batch-max-hours"
)

// The bounds of the AI service.
const (
	maxIdleMinutes    = 24 * 60 // a day
	maxReleaseHours   = 30 * 24 // 30 days
	maxScratchGB      = 200
	maxJobsPerPerson  = 1000
	maxJobHours       = 24 // workspace.MaxJobHours there
	batchPreemptible  = "preemptible"
	batchGuaranteed   = "guaranteed"
	groupReleaseCPU   = "Release times CPU"
	groupReleaseGPU   = "Release times GPU"
	groupWorkspaces   = "Workspaces"
	groupBatchJobs    = "Batch jobs"
	togetherBatchJobs = "batch-jobs"
	// What switching batch jobs on in a budget pre-fills (Cap.Default), the AI service's defaults for a project's batch jobs.
	defaultJobsPerPerson = 2
	defaultJobHours      = 4
)

// Caps are the caps the AI service declares. Name and Message are in German and English: the tree's validation messages name a cap by Name, and the UI shows both (in the language it asks for) where it has no translation of its own. Group and Choices stay as they are: they are keys, the UI translates a group by its name (groupSlug in util-caps.js) and a choice by its value, and a choice is what the AI service is told (BatchPriorityFor).
var Caps = []common.Cap{
	{ID: CapReleaseCPUIdleOpen, Name: msg.T("CPU: Leerlauf mit offenem Tab", "CPU: idle with an open tab"), Group: groupReleaseCPU, Unit: "min", Min: 1, Max: maxIdleMinutes, Message: msg.T("Minuten ohne Aktivität, solange ein Tab des Workspace offen ist", "Minutes without activity while a tab of the workspace is open")},
	{ID: CapReleaseCPUIdleClosed, Name: msg.T("CPU: Leerlauf ohne Tab", "CPU: idle without a tab"), Group: groupReleaseCPU, Unit: "min", Min: 1, Max: maxIdleMinutes, Message: msg.T("Minuten ohne Aktivität, sobald kein Tab mehr offen ist", "Minutes without activity once no tab is open")},
	{ID: CapReleaseCPUMaxHours, Name: msg.T("CPU: Obergrenze", "CPU: upper bound"), Group: groupReleaseCPU, Unit: "h", Min: 1, Max: maxReleaseHours, Message: msg.T("Stunden nach dem Start, nach denen ein Workspace spätestens endet", "Hours after the start after which a workspace ends at the latest")},
	{ID: CapReleaseGPUIdleOpen, Name: msg.T("GPU: Leerlauf mit offenem Tab", "GPU: idle with an open tab"), Group: groupReleaseGPU, Unit: "min", Min: 1, Max: maxIdleMinutes, Message: msg.T("Minuten ohne Aktivität, solange ein Tab des Workspace offen ist", "Minutes without activity while a tab of the workspace is open")},
	{ID: CapReleaseGPUIdleClosed, Name: msg.T("GPU: Leerlauf ohne Tab", "GPU: idle without a tab"), Group: groupReleaseGPU, Unit: "min", Min: 1, Max: maxIdleMinutes, Message: msg.T("Minuten ohne Aktivität, sobald kein Tab mehr offen ist", "Minutes without activity once no tab is open")},
	{ID: CapReleaseGPUMaxHours, Name: msg.T("GPU: Obergrenze", "GPU: upper bound"), Group: groupReleaseGPU, Unit: "h", Min: 1, Max: maxReleaseHours, Unbounded: true, Message: msg.T("Stunden nach dem Start, nach denen ein GPU-Workspace oder GPU-Kernel spätestens endet; 0 = keine (nur für Projekte mit eigenen GPUs)", "Hours after the start after which a GPU workspace or GPU kernel ends at the latest; 0 = none (only for projects with GPUs of their own)")},
	{ID: CapWorkspaceScratchGB, Name: msg.T("Lokaler Speicher je Workspace", "Local storage per workspace"), Group: groupWorkspaces, Unit: "GB", Min: 1, Max: maxScratchGB, Message: msg.T("Was ein Workspace oder GPU-Kernel außerhalb des Homes schreiben darf", "What a workspace or GPU kernel may write outside its home")},
	{ID: CapBatchPriority, Name: msg.T("Priorität der Batch-Jobs", "Priority of batch jobs"), Group: groupBatchJobs, Choices: []string{batchPreemptible, batchGuaranteed}, Message: msg.T("Ob Batch-Jobs verdrängt werden dürfen", "Whether batch jobs may be preempted")},
	{ID: CapBatchJobsPerPerson, Name: msg.T("Batch-Jobs je Person", "Batch jobs per person"), Group: groupBatchJobs, Min: 1, Max: maxJobsPerPerson, Grant: true, Together: togetherBatchJobs, Default: new(defaultJobsPerPerson), Message: msg.T("Batch-Jobs, die eine Person gleichzeitig wartend und laufend haben darf", "Batch jobs a person may have waiting and running at the same time")},
	{ID: CapBatchMaxHours, Name: msg.T("Laufzeit eines Batch-Jobs", "Run time of a batch job"), Group: groupBatchJobs, Unit: "h", Min: 1, Max: maxJobHours, Grant: true, Together: togetherBatchJobs, Default: new(defaultJobHours), Message: msg.T("Wie lange ein Batch-Job höchstens läuft", "How long a batch job runs at most")},
}

func init() {
	for i := range Caps {
		Caps[i].Provider = Provider
		Caps[i].Area = common.AreaWorkspaces
	}
}

// resolved is what applies below chain of the AI service's caps.
func resolved(chain []model.Node) common.Caps { return model.ResolveCaps(chain, Caps) }

func optional(c common.Caps, id string) *int {
	if v, ok := c[id]; ok {
		return &v
	}
	return nil
}

// ReleaseFor is what the AI service is told about a leaf's release times: the strictest of the budgets above it (chain, in any order). Never nil, also when no budget sets any: then every field is left out and the AI service applies its defaults.
func ReleaseFor(chain []model.Node) *Release {
	c := resolved(chain)
	return &Release{
		CPU: ReleaseTimes{IdleOpenMinutes: optional(c, CapReleaseCPUIdleOpen), IdleClosedMinutes: optional(c, CapReleaseCPUIdleClosed), MaxHours: optional(c, CapReleaseCPUMaxHours)},
		GPU: ReleaseTimes{IdleOpenMinutes: optional(c, CapReleaseGPUIdleOpen), IdleClosedMinutes: optional(c, CapReleaseGPUIdleClosed), MaxHours: optional(c, CapReleaseGPUMaxHours)},
	}
}

// ScratchFor is what the AI service is told about a leaf's local storage per workspace: the smallest value of the budgets above it, 0 when none sets one.
func ScratchFor(chain []model.Node) int { return resolved(chain)[CapWorkspaceScratchGB] }

// BatchPriorityFor is what the AI service is told about a leaf's batch priority: the strictest value of the budgets above it, empty when none sets one.
func BatchPriorityFor(chain []model.Node) string {
	v, ok := resolved(chain)[CapBatchPriority]
	if !ok {
		return ""
	}
	return Caps[capIndex(CapBatchPriority)].Label(v).String()
}

// BatchJobsFor is what the AI service is told about a leaf's batch jobs: what the budgets above it allow, {0, 0} where they have none. Never nil.
func BatchJobsFor(chain []model.Node) *BatchJobs {
	c := resolved(chain)
	return &BatchJobs{JobsPerPerson: c[CapBatchJobsPerPerson], MaxHours: c[CapBatchMaxHours]}
}

func capIndex(id string) int {
	for i, d := range Caps {
		if d.ID == id {
			return i
		}
	}
	panic("unknown cap " + id)
}

// Rules are the AI service's rules for the tree (tree.ProviderRules).
type Rules struct {
	// Members is how many members a project of the AI service may list.
	Members int
}

func (Rules) Provider() string  { return Provider }
func (r Rules) MaxMembers() int { return r.Members }
