package estimate

import (
	"strings"

	"github.com/pfisterer/cloud-self-service-golib/msg"
)

// Input is what the estimator computes from: any of the three areas, each with its own people and times.
type Input struct {
	// Level is typical (default) or high: the median of the profiles or their generous end.
	Level      Level           `json:"level,omitempty" enums:"typical,high" example:"typical"`
	LLM        *LLMInput       `json:"llm,omitempty"`
	Cloud      *CloudInput     `json:"cloud,omitempty"`
	Workspaces *WorkspaceInput `json:"workspaces,omitempty"`
}

// Schedule is when the people work: sessions per week of so many hours, for so many weeks; Together when they all work at the same time (a course session) rather than whenever they like.
type Schedule struct {
	SessionsPerWeek float64 `json:"sessions_per_week" example:"1"`
	HoursPerSession float64 `json:"hours_per_session" example:"3.5"`
	Weeks           float64 `json:"weeks" example:"12"`
	Together        bool    `json:"together" example:"true"`
}

// LLMInput describes use of the LLM gateway.
type LLMInput struct {
	// Tool is a profile id: chat, code, agent or script (GET /v1/estimate/options lists them).
	Tool    string `json:"tool" example:"code"`
	Persons int    `json:"persons" example:"20"`
	Schedule
	// Model overrides the tool's model alias, e.g. chat-large; it decides the price.
	Model string `json:"model,omitempty" example:"code"`
	// For own scripts (or to override a profile): requests per hour and tokens per request of one person or one script instance.
	RequestsPerHour        *float64 `json:"requests_per_hour,omitempty"`
	InputTokensPerRequest  *float64 `json:"input_tokens_per_request,omitempty"`
	OutputTokensPerRequest *float64 `json:"output_tokens_per_request,omitempty"`
}

// CloudInput is a composition of flavours and volumes, optionally per person of a course, optionally with the hours the machines run.
type CloudInput struct {
	Flavors []FlavorCount `json:"flavors"`
	Volumes []VolumeSpec  `json:"volumes,omitempty"`
	// PerPerson multiplies the composition by Participants (every participant of a course gets one).
	PerPerson    bool `json:"per_person,omitempty"`
	Participants int  `json:"participants,omitempty" example:"0"`
	// HoursPerWeek and Weeks the machines run, for the vCPU hours; optional.
	HoursPerWeek float64 `json:"hours_per_week,omitempty"`
	Weeks        float64 `json:"weeks,omitempty"`
}

// FlavorCount is a number of machines of one flavour, named by name or id.
type FlavorCount struct {
	Flavor string `json:"flavor" example:"m1.large"`
	Count  int    `json:"count" example:"3"`
}

// VolumeSpec is a number of volumes of one size.
type VolumeSpec struct {
	SizeGB int `json:"size_gb" example:"50"`
	Count  int `json:"count" example:"1"`
}

// WorkspaceInput describes workspaces in JupyterHub.
type WorkspaceInput struct {
	// Kind is a workspace kind id: cpu, cpu-gpukernel or gpu.
	Kind string `json:"kind" example:"cpu"`
	// GPUClass is the GPU class for the GPU kinds; optional when there is only one.
	GPUClass string `json:"gpu_class,omitempty"`
	Persons  int    `json:"persons" example:"20"`
	Schedule
	StorageGBPerPerson float64 `json:"storage_gb_per_person,omitempty" example:"5"`
	SharedStorageGB    float64 `json:"shared_storage_gb,omitempty" example:"0"`
}

// Bounds: generous for a university, small enough that nobody computes nonsense by a typo.
const (
	maxPersons         = 5000
	maxSessionsPerWeek = 21
	maxHoursPerSession = 24
	maxWeeks           = 104
	maxCount           = 1000
	maxVolumeGB        = 100000
	maxPerRequest      = 2_000_000
	maxRequestsPerHour = 100000
)

func invalid(de, en string, args ...any) error { return msg.Errorf(de, en, args...) }

// Validate checks an input before anything is computed; the message names the field.
func (in Input) Validate(cfg Config) error {
	if in.Level != "" && in.Level != LevelTypical && in.Level != LevelHigh {
		return invalid("level ist typical oder high.", "level is typical or high.")
	}
	if in.LLM == nil && in.Cloud == nil && in.Workspaces == nil {
		return invalid("Mindestens einer der Bereiche llm, cloud oder workspaces ist nötig.", "At least one of llm, cloud or workspaces is needed.")
	}
	if l := in.LLM; l != nil {
		if _, ok := cfg.LLM.Tools[l.Tool]; !ok {
			return invalid("llm.tool ist eines von %s.", "llm.tool is one of %s.", strings.Join(cfg.ToolIDs(), ", "))
		}
		if err := checkPeople("llm", l.Persons); err != nil {
			return err
		}
		if err := l.Schedule.check("llm"); err != nil {
			return err
		}
		if err := checkOptional("llm.requests_per_hour", l.RequestsPerHour, maxRequestsPerHour); err != nil {
			return err
		}
		if err := checkOptional("llm.input_tokens_per_request", l.InputTokensPerRequest, maxPerRequest); err != nil {
			return err
		}
		if err := checkOptional("llm.output_tokens_per_request", l.OutputTokensPerRequest, maxPerRequest); err != nil {
			return err
		}
	}
	if c := in.Cloud; c != nil {
		if len(c.Flavors) == 0 && len(c.Volumes) == 0 {
			return invalid("cloud braucht Flavors oder Volumes.", "cloud needs flavors or volumes.")
		}
		for _, f := range c.Flavors {
			if strings.TrimSpace(f.Flavor) == "" || f.Count < 1 || f.Count > maxCount {
				return invalid("cloud.flavors: jeder Eintrag braucht flavor und count zwischen 1 und %d.", "cloud.flavors: every entry needs flavor and a count between 1 and %d.", maxCount)
			}
		}
		for _, v := range c.Volumes {
			if v.SizeGB < 1 || v.SizeGB > maxVolumeGB || v.Count < 1 || v.Count > maxCount {
				return invalid("cloud.volumes: size_gb zwischen 1 und %d, count zwischen 1 und %d.", "cloud.volumes: size_gb between 1 and %d, count between 1 and %d.", maxVolumeGB, maxCount)
			}
		}
		if c.PerPerson {
			if err := checkPeople("cloud.participants", c.Participants); err != nil {
				return err
			}
		}
		if c.HoursPerWeek < 0 || c.HoursPerWeek > 168 || c.Weeks < 0 || c.Weeks > maxWeeks {
			return invalid("cloud.hours_per_week liegt zwischen 0 und 168, cloud.weeks zwischen 0 und %d.", "cloud.hours_per_week is between 0 and 168, cloud.weeks between 0 and %d.", maxWeeks)
		}
		if (c.HoursPerWeek > 0) != (c.Weeks > 0) {
			return invalid("cloud.hours_per_week und cloud.weeks gehören zusammen.", "cloud.hours_per_week and cloud.weeks go together.")
		}
	}
	if w := in.Workspaces; w != nil {
		if _, ok := cfg.Workspaces.Kinds[w.Kind]; !ok {
			return invalid("workspaces.kind ist eines von %s.", "workspaces.kind is one of %s.", strings.Join(cfg.WorkspaceKindIDs(), ", "))
		}
		if err := checkPeople("workspaces", w.Persons); err != nil {
			return err
		}
		if err := w.Schedule.check("workspaces"); err != nil {
			return err
		}
		if w.StorageGBPerPerson < 0 || w.StorageGBPerPerson > 1000 || w.SharedStorageGB < 0 || w.SharedStorageGB > maxVolumeGB {
			return invalid("workspaces: Speicher je Person zwischen 0 und 1000 GB, gemeinsamer Speicher zwischen 0 und %d GB.", "workspaces: storage per person between 0 and 1000 GB, shared storage between 0 and %d GB.", maxVolumeGB)
		}
	}
	return nil
}

func checkPeople(field string, n int) error {
	if n < 1 || n > maxPersons {
		return invalid("%s: Personen zwischen 1 und %d.", "%s: persons between 1 and %d.", field, maxPersons)
	}
	return nil
}

func (s Schedule) check(field string) error {
	if s.SessionsPerWeek <= 0 || s.SessionsPerWeek > maxSessionsPerWeek {
		return invalid("%s.sessions_per_week liegt über 0 und höchstens bei %d.", "%s.sessions_per_week is above 0 and at most %d.", field, maxSessionsPerWeek)
	}
	if s.HoursPerSession <= 0 || s.HoursPerSession > maxHoursPerSession {
		return invalid("%s.hours_per_session liegt über 0 und höchstens bei %d.", "%s.hours_per_session is above 0 and at most %d.", field, maxHoursPerSession)
	}
	if s.Weeks <= 0 || s.Weeks > maxWeeks {
		return invalid("%s.weeks liegt über 0 und höchstens bei %d.", "%s.weeks is above 0 and at most %d.", field, maxWeeks)
	}
	if s.SessionsPerWeek*s.HoursPerSession > 168 {
		return invalid("%s: mehr als 168 Stunden in der Woche.", "%s: more than 168 hours a week.", field)
	}
	return nil
}

func checkOptional(field string, v *float64, limit float64) error {
	if v != nil && (*v < 0 || *v > limit) {
		return invalid("%s liegt zwischen 0 und %v.", "%s is between 0 and %v.", field, limit)
	}
	return nil
}
