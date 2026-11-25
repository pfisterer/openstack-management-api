package estimate

import (
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/pfisterer/cloud-self-service-golib/msg"
	"github.com/pfisterer/openstack-management-api/internal/common"
)

// Env is what the estimate needs to know about the deployment and the person: the catalogue (which resource ids exist; empty = do not check) and the flavours, all of them and those the person may use.
type Env struct {
	Catalog        []common.Resource
	AllFlavors     []Flavor
	AllowedFlavors []Flavor
}

// Note is an assumption or a warning, in the caller's language.
type Note struct {
	Code string `json:"code" example:"llm_profile"`
	Text string `json:"text"`
}

// Result is the estimate: the proposed amounts by resource id, ready to fill a request form, the intermediate numbers of each area, and the assumptions behind them.
type Result struct {
	// Limit is what a project would ask for, by catalogue resource id (window resources per 30 days).
	Limit       common.ProjectQuota `json:"limit"`
	LLM         *LLMResult          `json:"llm,omitempty"`
	Cloud       *CloudResult        `json:"cloud,omitempty"`
	Workspaces  *WorkspaceResult    `json:"workspaces,omitempty"`
	Assumptions []Note              `json:"assumptions"`
	Warnings    []Note              `json:"warnings"`
	// ProfilesVersion is the date of the profiles the numbers come from.
	ProfilesVersion string `json:"profiles_version" example:"2026-11-25"`
}

// LLMResult are the steps of the LLM estimate.
type LLMResult struct {
	Tool                   string  `json:"tool"`
	Model                  string  `json:"model"`
	Level                  Level   `json:"level"`
	RequestsPerHour        float64 `json:"requests_per_hour"`
	InputTokensPerRequest  float64 `json:"input_tokens_per_request"`
	OutputTokensPerRequest float64 `json:"output_tokens_per_request"`
	TokensPerPersonHour    float64 `json:"tokens_per_person_hour"`
	PersonHoursPerWeek     float64 `json:"person_hours_per_week"`
	TokensPerWeek          float64 `json:"tokens_per_week"`
	// TokensPerWindow are input and output tokens in the busiest 30 days; TotalTokens over the whole duration.
	TokensPerWindow float64 `json:"tokens_per_window"`
	TotalTokens     float64 `json:"total_tokens"`
	// PoolMTokensPerWindow is what the busiest 30 days cost of the pool, in millions at the reference price, with headroom.
	PoolMTokensPerWindow float64 `json:"pool_mtokens_per_window"`
	ConcurrentPersons    int     `json:"concurrent_persons"`
	PeakTPMPerPerson     float64 `json:"peak_tpm_per_person"`
	ProjectTPM           int     `json:"project_tpm"`
	ProjectRPM           int     `json:"project_rpm"`
	// KeyWindowTokens is what one person uses in one key window (4 hours), KeyWindowPoolMTokens its cost of the pool; PoolMinForKeyWindow the pool at which a key's share covers that.
	KeyWindowHours       float64 `json:"key_window_hours"`
	KeyWindowTokens      float64 `json:"key_window_tokens"`
	KeyWindowPoolMTokens float64 `json:"key_window_pool_mtokens"`
	KeyShare             float64 `json:"key_share"`
	PoolMinForKeyWindow  float64 `json:"pool_min_for_key_window"`
	// MTokens is the proposed pool, whole millions per 30 days.
	MTokens int `json:"mtokens"`
}

// CloudResult are the sums of the composition.
type CloudResult struct {
	Multiplier int             `json:"multiplier"`
	Items      []CloudItem     `json:"items"`
	Cores      int             `json:"cores"`
	RAMGB      int             `json:"ram_gb"`
	StorageGB  int             `json:"storage_gb"`
	GPUs       int             `json:"gpus"`
	LocalDisk  int             `json:"local_disk_gb"`
	VCPUHours  *CloudVCPUHours `json:"vcpu_hours,omitempty"`
	// Availabilities are the private flavours' availabilities the composition needs.
	Availabilities []string `json:"availabilities,omitempty"`
}

type CloudItem struct {
	Flavor  Flavor `json:"flavor"`
	Count   int    `json:"count"`
	Allowed bool   `json:"allowed"`
}

type CloudVCPUHours struct {
	PerWeek   float64 `json:"per_week"`
	PerWindow float64 `json:"per_window"`
	Total     float64 `json:"total"`
}

// WorkspaceResult are the steps of the workspace estimate.
type WorkspaceResult struct {
	Kind               string  `json:"kind"`
	GPUClass           string  `json:"gpu_class,omitempty"`
	PersonHoursPerWeek float64 `json:"person_hours_per_week"`
	PersonHoursWindow  float64 `json:"person_hours_per_window"`
	VCPUHours          int     `json:"vcpu_hours"`
	RAMGBHours         int     `json:"ram_gb_hours"`
	GPUHours           int     `json:"gpu_hours"`
	ConcurrentGPUs     int     `json:"concurrent_gpus"`
	StorageGB          int     `json:"storage_gb"`
	TotalVCPUHours     float64 `json:"total_vcpu_hours"`
	TotalGPUHours      float64 `json:"total_gpu_hours"`
}

// Estimator computes estimates under one configuration.
type Estimator struct {
	cfg    Config
	pricer Pricer
}

// New builds an estimator; pricer nil prices by the configuration (ConfigPricer).
func New(cfg Config, pricer Pricer) *Estimator {
	if pricer == nil {
		pricer = ConfigPricer{Config: cfg.LLM.Pricing}
	}
	return &Estimator{cfg: cfg, pricer: pricer}
}

func (e *Estimator) Config() Config { return e.cfg }

// run collects the notes while computing.
type run struct {
	cfg         Config
	env         Env
	known       map[string]bool
	limit       common.ProjectQuota
	assumptions []noteText
	warnings    []noteText
}

type noteText struct {
	code string
	text msg.Text
}

func (r *run) assume(code, de, en string, args ...any) {
	r.assumptions = append(r.assumptions, noteText{code, msg.F(de, en, args...)})
}

func (r *run) warn(code, de, en string, args ...any) {
	r.warnings = append(r.warnings, noteText{code, msg.F(de, en, args...)})
}

// set writes an amount where the catalogue has the resource (or every id without a catalogue); a resource it lacks is a warning, not an error, so the rest of the estimate still serves.
func (r *run) set(id string, v int) {
	if id == "" {
		return
	}
	if r.known != nil && !r.known[id] {
		r.warn("unknown_resource", "Die Ressource %s gibt es hier nicht; der Wert %d wird nicht vorgeschlagen.", "The resource %s does not exist here; the value %d is not proposed.", id, v)
		return
	}
	r.limit[id] = v
}

// Estimate computes the estimate of in under env, with its notes in lang. The input is validated first.
func (e *Estimator) Estimate(in Input, env Env, lang msg.Lang) (Result, error) {
	if err := in.Validate(e.cfg); err != nil {
		return Result{}, err
	}
	r := &run{cfg: e.cfg, env: env, limit: common.ProjectQuota{}}
	if len(env.Catalog) > 0 {
		r.known = map[string]bool{}
		for _, res := range env.Catalog {
			if !res.Withdrawn {
				r.known[res.ID] = true
			}
		}
	}
	level := in.Level
	if level == "" {
		level = LevelTypical
	}
	out := Result{ProfilesVersion: e.cfg.Version}
	if in.LLM != nil {
		out.LLM = e.llm(r, *in.LLM, level)
	}
	if in.Cloud != nil {
		res, err := e.cloud(r, *in.Cloud)
		if err != nil {
			return Result{}, err
		}
		out.Cloud = res
	}
	if in.Workspaces != nil {
		out.Workspaces = e.workspaces(r, *in.Workspaces)
	}
	out.Limit = r.limit
	out.Assumptions = render(r.assumptions, lang)
	out.Warnings = render(r.warnings, lang)
	return out, nil
}

func render(ns []noteText, lang msg.Lang) []Note {
	out := make([]Note, 0, len(ns))
	for _, n := range ns {
		t := n.text.In(lang)
		if lang == msg.DE {
			t = germanDecimals(t)
		}
		out = append(out, Note{Code: n.code, Text: t})
	}
	return out
}

// decimalPoint is a decimal number standing on its own (after a space, an opening bracket, ×, = or at the start), not part of a name like qwen3.8.
var decimalPoint = regexp.MustCompile(`(^|[\s(×=~])(\d+)\.(\d+)`)

// germanDecimals writes the decimal numbers of a German note with a comma.
func germanDecimals(s string) string {
	return decimalPoint.ReplaceAllString(s, "${1}${2},${3}")
}

// windowWeeks is how many of the weeks fall into the busiest window (30 days).
func (e *Estimator) windowWeeks(weeks float64) float64 {
	return math.Min(weeks, e.cfg.Resources.WindowDays/7)
}

// ceilInt rounds up, tolerating float noise (3.0000000001 is 3).
func ceilInt(v float64) int {
	return int(math.Ceil(v - 1e-9))
}

func roundUpTo(v float64, step int) int {
	if step <= 1 {
		return ceilInt(v)
	}
	return ceilInt(v/float64(step)) * step
}

func (e *Estimator) llm(r *run, in LLMInput, level Level) *LLMResult {
	t := e.cfg.LLM.Tools[in.Tool]
	model := strings.TrimSpace(in.Model)
	if model == "" {
		model = t.Model
	}
	res := &LLMResult{Tool: in.Tool, Model: model, Level: level,
		RequestsPerHour:        pick(in.RequestsPerHour, t.RequestsPerHour.At(level)),
		InputTokensPerRequest:  pick(in.InputTokensPerRequest, t.InputTokensPerRequest),
		OutputTokensPerRequest: pick(in.OutputTokensPerRequest, t.OutputTokensPerRequest),
	}
	perReq := res.InputTokensPerRequest + res.OutputTokensPerRequest
	res.TokensPerPersonHour = res.RequestsPerHour * perReq
	res.PersonHoursPerWeek = float64(in.Persons) * in.SessionsPerWeek * in.HoursPerSession
	res.TokensPerWeek = res.PersonHoursPerWeek * res.TokensPerPersonHour
	ww := e.windowWeeks(in.Weeks)
	res.TokensPerWindow = res.TokensPerWeek * ww
	res.TotalTokens = res.TokensPerWeek * in.Weeks
	inWin := res.PersonHoursPerWeek * ww * res.RequestsPerHour * res.InputTokensPerRequest
	outWin := res.PersonHoursPerWeek * ww * res.RequestsPerHour * res.OutputTokensPerRequest
	res.PoolMTokensPerWindow = e.pricer.PoolMTokens(model, inWin, outWin) * e.cfg.LLM.TokenHeadroom

	r.assume("llm_profile", "Werkzeug %s (Modell %s): %.0f Anfragen je Person und Stunde mit je %.0f Eingabe- und %.0f Ausgabe-Token, also etwa %.0f Token je Personenstunde (Profil vom %s, Stufe %s).",
		"Tool %s (model %s): %.0f requests per person and hour of %.0f input and %.0f output tokens each, about %.0f tokens per person-hour (profiles of %s, level %s).",
		in.Tool, model, res.RequestsPerHour, res.InputTokensPerRequest, res.OutputTokensPerRequest, res.TokensPerPersonHour, e.cfg.Version, level)
	r.assume("llm_time", "%d Personen × %.4g Termine/Woche × %.4g h = %.4g Personenstunden je Woche; in den 30 Tagen mit der meisten Nutzung %.4g Wochen, insgesamt %.4g Wochen.",
		"%d people × %.4g sessions/week × %.4g h = %.4g person-hours per week; %.4g weeks in the busiest 30 days, %.4g weeks in all.",
		in.Persons, in.SessionsPerWeek, in.HoursPerSession, res.PersonHoursPerWeek, ww, in.Weeks)
	r.assume("llm_pool", "Der Pool zählt je 30 Tage und in Geld zum Referenzpreis: Ausgabe-Token und größere Modelle kosten mehr als ein Token des Pools; dazu %.0f %% Spielraum.",
		"The pool counts per 30 days and in money at the reference price: output tokens and larger models cost more than one pool token; plus %.0f %% headroom.",
		(e.cfg.LLM.TokenHeadroom-1)*100)

	// Rates: who is active in the same minute, each at their busiest minute.
	if in.Together {
		res.ConcurrentPersons = in.Persons
		r.assume("llm_concurrency", "Alle %d Personen arbeiten gleichzeitig (Termin).", "All %d people work at the same time (session).", in.Persons)
	} else {
		res.ConcurrentPersons = max(1, ceilInt(float64(in.Persons)*t.ConcurrentShare))
		r.assume("llm_concurrency", "Nicht alle arbeiten gleichzeitig: %.0f %% der Personen in derselben Minute aktiv, also %d.", "Not everybody works at the same time: %.0f %% of the people active in the same minute, i.e. %d.", t.ConcurrentShare*100, res.ConcurrentPersons)
	}
	// A burst of two requests in a minute is the least a person at work sends; the profile's peak covers the tools that send more.
	res.PeakTPMPerPerson = math.Max(t.PeakTPMPerPerson.At(level), res.RequestsPerHour/60*perReq*2)
	res.ProjectTPM = roundUpTo(float64(res.ConcurrentPersons)*res.PeakTPMPerPerson*e.cfg.LLM.RateHeadroom, e.cfg.LLM.TPMRoundTo)
	rpmPerPerson := math.Max(e.cfg.LLM.RPMPerPersonPeak.At(level), res.RequestsPerHour/60*2)
	res.ProjectRPM = ceilInt(float64(res.ConcurrentPersons) * rpmPerPerson * e.cfg.LLM.RateHeadroom)
	r.assume("llm_rates", "Token pro Minute: %d gleichzeitig Aktive × %.0f in der stärksten Minute × %.3g Spielraum; Anfragen pro Minute ebenso mit %.3g je Person.",
		"Tokens per minute: %d active at once × %.0f in their busiest minute × %.3g headroom; requests per minute likewise with %.3g per person.",
		res.ConcurrentPersons, res.PeakTPMPerPerson, e.cfg.LLM.RateHeadroom, rpmPerPerson)

	// Key window: one person's use in a session, as far as it falls into one window.
	kw := e.cfg.LLM.KeyWindow
	res.KeyWindowHours = math.Min(in.HoursPerSession, kw.Hours)
	res.KeyShare = kw.Share
	res.KeyWindowTokens = res.KeyWindowHours * res.TokensPerPersonHour
	res.KeyWindowPoolMTokens = e.pricer.PoolMTokens(model, res.KeyWindowHours*res.RequestsPerHour*res.InputTokensPerRequest, res.KeyWindowHours*res.RequestsPerHour*res.OutputTokensPerRequest) * e.cfg.LLM.TokenHeadroom
	res.PoolMinForKeyWindow = res.KeyWindowPoolMTokens / kw.Share
	res.MTokens = max(1, ceilInt(math.Max(res.PoolMTokensPerWindow, res.PoolMinForKeyWindow)))
	r.assume("llm_key_window", "Je Schlüssel gilt ein Anteil von %.0f %% des Pools je %.0f Stunden; eine Person braucht in %.3g Stunden etwa %.0f Token (%.3g Mio. des Pools).",
		"Every key may spend %.0f %% of the pool per %.0f hours; one person needs about %.0f tokens in %.3g hours (%.3g M of the pool).",
		kw.Share*100, kw.Hours, res.KeyWindowHours, res.KeyWindowTokens, res.KeyWindowPoolMTokens)
	if res.PoolMinForKeyWindow > res.PoolMTokensPerWindow {
		r.warn("llm_key_window_raises_pool", "Damit der Anteil eines Schlüssels für eine Sitzung reicht, braucht der Pool mindestens %.3g Mio. statt %.3g Mio.; vorgeschlagen ist der größere Wert. Alternativ den Anteil je Schlüssel erhöhen lassen.",
			"For a key's share to cover one session the pool needs at least %.3g M instead of %.3g M; the larger value is proposed. Alternatively ask for a larger share per key.",
			res.PoolMinForKeyWindow, res.PoolMTokensPerWindow)
	}

	ids := e.cfg.Resources
	r.set(ids.LLMMTokens, res.MTokens)
	r.set(ids.LLMTPM, res.ProjectTPM)
	r.set(ids.LLMRPM, res.ProjectRPM)
	if id := e.modelAvailability(r, model); id != "" {
		r.set(id, 1)
		r.assume("llm_model", "Das Modell %s braucht die Freigabe %s.", "The model %s needs the availability %s.", model, id)
	}
	if t.Source != "" && strings.HasPrefix(t.Source, "placeholder") && in.RequestsPerHour == nil {
		r.warn("llm_script_numbers", "Für eigene Skripte sind die Profilzahlen nur Platzhalter; besser Anfragen pro Stunde und Token je Anfrage angeben.", "For own scripts the profile's numbers are placeholders; better give requests per hour and tokens per request.")
	}
	return res
}

// modelAvailability is the catalogue availability that grants model (llm-model-<model>, or one naming it in its models); empty for the stable aliases every LLM project has.
func (e *Estimator) modelAvailability(r *run, model string) string {
	prefix := e.cfg.Resources.LLMModelPrefix
	for _, res := range r.env.Catalog {
		if res.Withdrawn || !res.IsBool() || !strings.HasPrefix(res.ID, prefix) {
			continue
		}
		if res.ID == prefix+model || slices.Contains(res.Models, model) {
			return res.ID
		}
	}
	return ""
}

func pick(v *float64, def float64) float64 {
	if v != nil {
		return *v
	}
	return def
}

func (e *Estimator) cloud(r *run, in CloudInput) (*CloudResult, error) {
	res := &CloudResult{Multiplier: 1}
	if in.PerPerson {
		res.Multiplier = in.Participants
	}
	var cores, ramMB, disk, gpus int
	avail := map[string]bool{}
	if len(in.Flavors) > 0 && len(r.env.AllFlavors) == 0 {
		return nil, invalid("Die Flavors lassen sich gerade nicht abrufen (OpenStack ist nicht verbunden); Kerne, RAM und Speicher bitte direkt angeben.", "The flavors cannot be fetched right now (OpenStack is not connected); please give cores, RAM and storage directly.")
	}
	for _, fc := range in.Flavors {
		f, ok := findFlavor(r.env.AllFlavors, fc.Flavor)
		if !ok {
			names := make([]string, 0, len(r.env.AllowedFlavors))
			for _, a := range r.env.AllowedFlavors {
				names = append(names, a.Name)
			}
			return nil, invalid("Den Flavor %q gibt es nicht. Möglich: %s.", "The flavor %q does not exist. Possible: %s.", fc.Flavor, strings.Join(names, ", "))
		}
		allowed := false
		if a, ok := findFlavor(r.env.AllowedFlavors, f.ID); ok {
			allowed, f = true, a
		}
		if !allowed {
			r.warn("flavor_not_allowed", "Der Flavor %s ist hier nicht freigegeben (kein Budget gibt ihn); er ist mitgerechnet, ließe sich aber nicht nutzen.", "The flavor %s is not available here (no budget grants it); it is counted, but could not be used.", f.Name)
		}
		res.Items = append(res.Items, CloudItem{Flavor: f, Count: fc.Count, Allowed: allowed})
		cores += f.VCPUs * fc.Count
		ramMB += f.RAMMB * fc.Count
		disk += f.DiskGB * fc.Count
		gpus += f.GPUs * fc.Count
		if f.Resource != "" && allowed {
			avail[f.Resource] = true
		}
	}
	storage := 0
	for _, v := range in.Volumes {
		storage += v.SizeGB * v.Count
	}
	m := res.Multiplier
	h := e.cfg.Cloud.Headroom
	res.Cores = ceilInt(float64(cores*m) * h)
	res.RAMGB = ceilInt(float64(ramMB*m) / 1024 * h)
	res.StorageGB = ceilInt(float64(storage*m) * h)
	res.GPUs = gpus * m
	res.LocalDisk = disk * m
	if in.PerPerson {
		r.assume("cloud_per_person", "Die Zusammenstellung gilt je Person: × %d Teilnehmende.", "The composition is per person: × %d participants.", in.Participants)
	}
	if res.LocalDisk > 0 {
		r.assume("cloud_local_disk", "Die Platten der Flavors (%d GB) liegen lokal und zählen nicht zum Speicher (Volumes); wer von einem Volume startet, braucht dessen Größe zusätzlich als Speicher.", "The flavors' disks (%d GB) are local and do not count as storage (volumes); booting from a volume needs its size as storage in addition.", res.LocalDisk)
	}
	if h > 1 {
		r.assume("cloud_headroom", "%.0f %% Spielraum auf Kerne, RAM und Speicher.", "%.0f %% headroom on cores, RAM and storage.", (h-1)*100)
	}
	if in.HoursPerWeek > 0 {
		perWeek := float64(cores*m) * in.HoursPerWeek
		res.VCPUHours = &CloudVCPUHours{PerWeek: perWeek, PerWindow: perWeek * e.windowWeeks(in.Weeks), Total: perWeek * in.Weeks}
		r.assume("cloud_vcpu_hours", "OpenStack begrenzt Kerne, nicht Stunden; die vCPU-Stunden (%.4g h/Woche × %.4g Wochen) sind zur Information, z. B. für die Verbrauchshistorie.", "OpenStack limits cores, not hours; the vCPU hours (%.4g h/week × %.4g weeks) are for information, e.g. the usage history.", in.HoursPerWeek, in.Weeks)
	}
	ids := e.cfg.Resources
	if len(in.Flavors) > 0 {
		r.set(ids.Cores, res.Cores)
		r.set(ids.RAMGB, res.RAMGB)
	}
	if len(in.Volumes) > 0 {
		r.set(ids.StorageGB, res.StorageGB)
	}
	for _, id := range sortedKeys(avail) {
		res.Availabilities = append(res.Availabilities, id)
		r.set(id, 1)
	}
	return res, nil
}

func (e *Estimator) workspaces(r *run, in WorkspaceInput) *WorkspaceResult {
	k := e.cfg.Workspaces.Kinds[in.Kind]
	ids := e.cfg.Resources
	h := e.cfg.Workspaces.Headroom
	res := &WorkspaceResult{Kind: in.Kind}
	res.PersonHoursPerWeek = float64(in.Persons) * in.SessionsPerWeek * in.HoursPerSession
	res.PersonHoursWindow = res.PersonHoursPerWeek * e.windowWeeks(in.Weeks)
	res.VCPUHours = ceilInt(res.PersonHoursWindow * k.VCPU * h)
	res.RAMGBHours = ceilInt(res.PersonHoursWindow * k.RAMGB * h)
	res.TotalVCPUHours = res.PersonHoursPerWeek * in.Weeks * k.VCPU
	res.StorageGB = ceilInt(float64(in.Persons)*in.StorageGBPerPerson + in.SharedStorageGB)
	r.assume("ws_kind", "Workspace-Art %s: gebucht werden %.3g Kerne und %.3g GB je laufendem Workspace und Stunde (zugesicherte Werte).", "Workspace kind %s: %.3g cores and %.3g GB are booked per running workspace and hour (guaranteed values).", in.Kind, k.VCPU, k.RAMGB)
	r.assume("ws_time", "%d Personen × %.4g Termine/Woche × %.4g h = %.4g Workspace-Stunden je Woche, in 30 Tagen %.4g; dazu %.0f %% Spielraum.", "%d people × %.4g sessions/week × %.4g h = %.4g workspace hours per week, %.4g in 30 days; plus %.0f %% headroom.",
		in.Persons, in.SessionsPerWeek, in.HoursPerSession, res.PersonHoursPerWeek, res.PersonHoursWindow, (h-1)*100)
	r.set(ids.WSVCPUHours, res.VCPUHours)
	r.set(ids.WSRAMGBHours, res.RAMGBHours)
	if res.StorageGB > 0 {
		r.set(ids.WSStorageGB, res.StorageGB)
	}
	if k.Profile != "" {
		r.set(ids.WSProfilePrefix+k.Profile, 1)
	}
	if k.GPUs > 0 {
		class := e.gpuClass(r, in.GPUClass)
		res.GPUClass = class
		res.GPUHours = ceilInt(res.PersonHoursWindow * k.GPUs * k.GPUTimeShare * h)
		res.TotalGPUHours = res.PersonHoursPerWeek * in.Weeks * k.GPUs * k.GPUTimeShare
		if in.Together {
			res.ConcurrentGPUs = ceilInt(float64(in.Persons) * k.GPUs)
		} else {
			res.ConcurrentGPUs = max(1, ceilInt(float64(in.Persons)*k.GPUs*e.cfg.Workspaces.ConcurrentShare))
		}
		if in.Together {
			r.assume("ws_gpu", "GPU belegt %.0f %% der Zeit; alle arbeiten gleichzeitig, also %d GPUs gleichzeitig.", "GPU held %.0f %% of the time; everybody works at the same time, so %d GPUs at once.", k.GPUTimeShare*100, res.ConcurrentGPUs)
		} else {
			r.assume("ws_gpu", "GPU belegt %.0f %% der Zeit; %.0f %% der Personen gleichzeitig, also %d GPUs gleichzeitig.", "GPU held %.0f %% of the time; %.0f %% of the people at once, so %d GPUs at once.", k.GPUTimeShare*100, e.cfg.Workspaces.ConcurrentShare*100, res.ConcurrentGPUs)
		}
		if class != "" {
			r.set(ids.WSGPUHoursPrefix+class, res.GPUHours)
			r.set(ids.WSGPUsPrefix+class, res.ConcurrentGPUs)
		}
	}
	return res
}

// gpuClass is the class asked for, or the only one the catalogue has; empty with a warning otherwise.
func (e *Estimator) gpuClass(r *run, want string) string {
	prefix := e.cfg.Resources.WSGPUsPrefix
	var classes []string
	for _, res := range r.env.Catalog {
		if !res.Withdrawn && strings.HasPrefix(res.ID, prefix) {
			classes = append(classes, strings.TrimPrefix(res.ID, prefix))
		}
	}
	want = strings.TrimSpace(want)
	switch {
	case want != "" && (len(r.env.Catalog) == 0 || slices.Contains(classes, want)):
		return want
	case want != "":
		r.warn("ws_gpu_class_unknown", "Die GPU-Klasse %s gibt es nicht; möglich: %s.", "The GPU class %s does not exist; possible: %s.", want, strings.Join(classes, ", "))
	case len(classes) == 1:
		r.assume("ws_gpu_class", "GPU-Klasse %s (die einzige).", "GPU class %s (the only one).", classes[0])
		return classes[0]
	default:
		r.warn("ws_gpu_class_missing", "Für GPU-Workspaces fehlt die GPU-Klasse; möglich: %s. GPU-Stunden sind berechnet, aber keiner Klasse zugeordnet.", "GPU workspaces need a GPU class; possible: %s. GPU hours are computed but assigned to no class.", strings.Join(classes, ", "))
	}
	return ""
}
