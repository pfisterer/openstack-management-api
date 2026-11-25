package estimate

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/msg"
	"github.com/pfisterer/openstack-management-api/internal/common"
)

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6*math.Max(1, math.Abs(want)) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func hasNote(ns []Note, code string) bool {
	for _, n := range ns {
		if n.Code == code {
			return true
		}
	}
	return false
}

// testCatalog is the resources of the deployment the estimate writes into, plus a model availability and one GPU class.
var testCatalog = []common.Resource{
	{ID: "cores"}, {ID: "ram"}, {ID: "storage"},
	{ID: "llm-mtokens"}, {ID: "llm-tpm"}, {ID: "llm-rpm"},
	{ID: "llm-model-chat-large", Kind: common.KindBool},
	{ID: "llm-model-flash-next", Kind: common.KindBool, Models: []string{"qwen3.8-flash-next-gyro-gyro-s"}},
	{ID: "ws-vcpu-hours"}, {ID: "ws-ram-gb-hours"}, {ID: "ws-storage-gb"},
	{ID: "ws-profile-cpu", Kind: common.KindBool}, {ID: "ws-profile-gpu", Kind: common.KindBool}, {ID: "ws-profile-cpu-gpukernel", Kind: common.KindBool},
	{ID: "ws-gpus-l4", Kind: common.KindCount}, {ID: "ws-gpu-hours-l4"},
	{ID: "flavor-gpu-l4", Kind: common.KindBool},
}

var testFlavors = []Flavor{
	{ID: "f-small", Name: "m1.small", VCPUs: 1, RAMMB: 2048, DiskGB: 20, Public: true},
	{ID: "f-large", Name: "m1.large", VCPUs: 4, RAMMB: 8192, DiskGB: 40, Public: true},
	{ID: "f-gpu", Name: "g1.l4", VCPUs: 8, RAMMB: 32768, DiskGB: 100, GPUs: 1},
	{ID: "f-secret", Name: "x1.huge", VCPUs: 64, RAMMB: 262144},
}

var testGrants = map[string]string{"f-gpu": "flavor-gpu-l4"}

func env(held map[string]bool) Env {
	return Env{Catalog: testCatalog, AllFlavors: testFlavors, AllowedFlavors: AllowedFlavors(testFlavors, testGrants, held)}
}

func TestDefaultConfigIsValid(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Version == "" || cfg.Source == "" {
		t.Fatal("the profiles say where their numbers come from and of when")
	}
	for _, id := range []string{"chat", "code", "agent", "script"} {
		tp, ok := cfg.LLM.Tools[id]
		if !ok {
			t.Fatalf("tool %s missing", id)
		}
		if tp.Source == "" {
			t.Errorf("tool %s has no source", id)
		}
	}
}

func TestLoadConfigFromFileAndRefusals(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "p.json")
	if err := os.WriteFile(good, defaultProfiles, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(good); err != nil {
		t.Fatalf("built-in profiles from a file: %v", err)
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("a missing file is an error, not the defaults")
	}
	bad := strings.Replace(string(defaultProfiles), `"token_headroom": 1.2`, `"token_headroom": 0.5`, 1)
	if _, err := ParseConfig([]byte(bad)); err == nil {
		t.Fatal("a headroom below 1 is refused")
	}
	if cfg, err := LoadConfig(""); err != nil || cfg.Version != DefaultConfig().Version {
		t.Fatal("no path is the built-in configuration")
	}
}

// The course that started it: "Datenanalyse mit R", 20 students with Positron, Mondays 3.5 hours, 12 weeks, all at once.
func TestLLMCourseWithCodingAssistant(t *testing.T) {
	e := New(DefaultConfig(), nil)
	res, err := e.Estimate(Input{LLM: &LLMInput{Tool: "code", Persons: 20, Schedule: Schedule{SessionsPerWeek: 1, HoursPerSession: 3.5, Weeks: 12, Together: true}}}, env(nil), msg.DE)
	if err != nil {
		t.Fatal(err)
	}
	l := res.LLM
	near(t, "tokens per person-hour", l.TokensPerPersonHour, 10*13200)
	near(t, "person-hours per week", l.PersonHoursPerWeek, 70)
	near(t, "tokens per week", l.TokensPerWeek, 70*132000)
	near(t, "tokens per window", l.TokensPerWindow, 70*132000*30.0/7)
	near(t, "total tokens", l.TotalTokens, 70*132000*12)
	// input 39 M at the reference price, output 0.6 M at three times it, 20 % headroom
	near(t, "pool per window", l.PoolMTokensPerWindow, (70*30.0/7*10*13000/1e6+70*30.0/7*10*200*3/1e6)*1.2)
	if l.MTokens != 49 {
		t.Errorf("mtokens = %d, want 49", l.MTokens)
	}
	if l.ConcurrentPersons != 20 {
		t.Errorf("concurrent = %d, want all 20", l.ConcurrentPersons)
	}
	// 20 × 25 000 × 1.25 = 625 000, rounded up to 10 000
	if l.ProjectTPM != 630000 {
		t.Errorf("tpm = %d, want 630000", l.ProjectTPM)
	}
	if l.ProjectRPM != 100 {
		t.Errorf("rpm = %d, want 100", l.ProjectRPM)
	}
	near(t, "key window hours", l.KeyWindowHours, 3.5)
	near(t, "key window tokens", l.KeyWindowTokens, 3.5*132000)
	if hasNote(res.Warnings, "llm_key_window_raises_pool") {
		t.Error("twenty people's pool covers a key's share of a session")
	}
	want := common.ProjectQuota{"llm-mtokens": 49, "llm-tpm": 630000, "llm-rpm": 100}
	for k, v := range want {
		if res.Limit[k] != v {
			t.Errorf("limit[%s] = %d, want %d", k, res.Limit[k], v)
		}
	}
	if len(res.Limit) != len(want) {
		t.Errorf("limit = %v, want only %v (code needs no model availability)", res.Limit, want)
	}
	if !hasNote(res.Assumptions, "llm_profile") || !hasNote(res.Assumptions, "llm_key_window") {
		t.Errorf("assumptions missing: %+v", res.Assumptions)
	}
	if !strings.Contains(res.Assumptions[0].Text, "Werkzeug") {
		t.Errorf("assumptions in German by default: %q", res.Assumptions[0].Text)
	}
}

func TestLLMHighLevelAndSpreadUse(t *testing.T) {
	e := New(DefaultConfig(), nil)
	res, err := e.Estimate(Input{Level: LevelHigh, LLM: &LLMInput{Tool: "code", Persons: 10, Schedule: Schedule{SessionsPerWeek: 2, HoursPerSession: 2, Weeks: 2}}}, env(nil), msg.EN)
	if err != nil {
		t.Fatal(err)
	}
	l := res.LLM
	near(t, "high requests per hour", l.RequestsPerHour, 25)
	// 2 weeks < 30 days: the window holds the whole duration
	near(t, "window = total", l.TokensPerWindow, l.TotalTokens)
	// 30 % of 10 at once
	if l.ConcurrentPersons != 3 {
		t.Errorf("concurrent = %d, want 3", l.ConcurrentPersons)
	}
	// 3 × 50 000 × 1.25 = 187 500 → 190 000
	if l.ProjectTPM != 190000 {
		t.Errorf("tpm = %d, want 190000", l.ProjectTPM)
	}
	if !strings.Contains(res.Assumptions[0].Text, "Tool") {
		t.Errorf("assumptions in English when asked: %q", res.Assumptions[0].Text)
	}
}

// One person chatting two hours a week: the pool for the month is small, but a key may spend only 5 % of it per 4 hours, so the pool is raised to cover a session.
func TestLLMKeyWindowRaisesSmallPool(t *testing.T) {
	e := New(DefaultConfig(), nil)
	res, err := e.Estimate(Input{LLM: &LLMInput{Tool: "chat", Persons: 1, Schedule: Schedule{SessionsPerWeek: 1, HoursPerSession: 2, Weeks: 4}}}, env(nil), msg.DE)
	if err != nil {
		t.Fatal(err)
	}
	l := res.LLM
	near(t, "pool per window", l.PoolMTokensPerWindow, (2*4*10*4000.0+2*4*10*800*3)/1e6*1.2)
	near(t, "key window pool", l.KeyWindowPoolMTokens, (2*10*4000.0+2*10*800*3)/1e6*1.2)
	near(t, "pool for the key window", l.PoolMinForKeyWindow, (2*10*4000.0+2*10*800*3)/1e6*1.2/0.05)
	if l.MTokens != 4 {
		t.Errorf("mtokens = %d, want 4 (3.07 rounded up)", l.MTokens)
	}
	if !hasNote(res.Warnings, "llm_key_window_raises_pool") {
		t.Error("the raise is explained")
	}
	if l.ConcurrentPersons != 1 {
		t.Errorf("at least one person is active: %d", l.ConcurrentPersons)
	}
}

func TestLLMScriptOverridesAndModelAvailability(t *testing.T) {
	e := New(DefaultConfig(), nil)
	rph, in, out := 600.0, 2000.0, 500.0
	res, err := e.Estimate(Input{LLM: &LLMInput{Tool: "script", Persons: 1, Model: "chat-large", RequestsPerHour: &rph, InputTokensPerRequest: &in, OutputTokensPerRequest: &out,
		Schedule: Schedule{SessionsPerWeek: 7, HoursPerSession: 24, Weeks: 52, Together: true}}}, env(nil), msg.DE)
	if err != nil {
		t.Fatal(err)
	}
	l := res.LLM
	near(t, "tokens per hour", l.TokensPerPersonHour, 600*2500)
	// chat-large costs three times: 168 h × 30/7 weeks × 600 × (2000×3 + 500×9) / 1e6 × 1.2
	near(t, "pool", l.PoolMTokensPerWindow, 168*30.0/7*600*(2000*3+500*9)/1e6*1.2)
	// the script's own rate exceeds the profile's peak: 600/60 × 2500 × 2 = 50 000 × 1.25
	if l.ProjectTPM != 70000 {
		t.Errorf("tpm = %d, want 70000 (62 500 rounded up)", l.ProjectTPM)
	}
	if res.Limit["llm-model-chat-large"] != 1 {
		t.Errorf("chat-large needs its availability: %v", res.Limit)
	}
	if hasNote(res.Warnings, "llm_script_numbers") {
		t.Error("no placeholder warning when the script's numbers are given")
	}
	res, err = e.Estimate(Input{LLM: &LLMInput{Tool: "script", Persons: 1, Model: "qwen3.8-flash-next-gyro-gyro-s", Schedule: Schedule{SessionsPerWeek: 1, HoursPerSession: 1, Weeks: 1}}}, env(nil), msg.DE)
	if err != nil {
		t.Fatal(err)
	}
	if res.Limit["llm-model-flash-next"] != 1 {
		t.Errorf("a real model name finds the availability that names it: %v", res.Limit)
	}
	if !hasNote(res.Warnings, "llm_script_numbers") {
		t.Error("placeholder numbers for scripts are pointed out")
	}
}

func TestPricerReferencePrice(t *testing.T) {
	p := ConfigPricer{Config: DefaultConfig().LLM.Pricing}
	near(t, "1 M input of code", p.PoolMTokens("code", 1e6, 0), 1)
	near(t, "1 M output of code", p.PoolMTokens("code", 0, 1e6), 3)
	near(t, "1 M input of chat-large", p.PoolMTokens("chat-large", 1e6, 0), 3)
	// A pricer of its own (the price fix's reference price) replaces the configured one.
	e := New(DefaultConfig(), fixedPricer(2))
	res, err := e.Estimate(Input{LLM: &LLMInput{Tool: "code", Persons: 1, Schedule: Schedule{SessionsPerWeek: 1, HoursPerSession: 1, Weeks: 1}}}, env(nil), msg.DE)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "pool by the plugged pricer", res.LLM.PoolMTokensPerWindow, 2*1.2)
}

type fixedPricer float64

func (f fixedPricer) PoolMTokens(string, float64, float64) float64 { return float64(f) }

func TestCloudComposition(t *testing.T) {
	e := New(DefaultConfig(), nil)
	res, err := e.Estimate(Input{Cloud: &CloudInput{Flavors: []FlavorCount{{Flavor: "m1.large", Count: 3}}, Volumes: []VolumeSpec{{SizeGB: 50, Count: 1}}}}, env(nil), msg.DE)
	if err != nil {
		t.Fatal(err)
	}
	c := res.Cloud
	if c.Cores != 12 || c.RAMGB != 24 || c.StorageGB != 50 || c.LocalDisk != 120 || c.GPUs != 0 {
		t.Errorf("3 × m1.large + 50 GB = %+v", c)
	}
	if res.Limit["cores"] != 12 || res.Limit["ram"] != 24 || res.Limit["storage"] != 50 {
		t.Errorf("limit = %v", res.Limit)
	}
	if c.VCPUHours != nil {
		t.Error("no hours, no vCPU hours")
	}
	if !hasNote(res.Assumptions, "cloud_local_disk") {
		t.Error("local disks are explained")
	}
}

func TestCloudPerPersonWithHoursAndPrivateFlavor(t *testing.T) {
	e := New(DefaultConfig(), nil)
	in := Input{Cloud: &CloudInput{Flavors: []FlavorCount{{Flavor: "M1.SMALL", Count: 1}, {Flavor: "f-gpu", Count: 1}}, PerPerson: true, Participants: 20, HoursPerWeek: 4, Weeks: 12}}
	res, err := e.Estimate(in, env(map[string]bool{"flavor-gpu-l4": true}), msg.DE)
	if err != nil {
		t.Fatal(err)
	}
	c := res.Cloud
	if c.Multiplier != 20 || c.Cores != 180 || c.RAMGB != 680 || c.GPUs != 20 || c.StorageGB != 0 {
		t.Errorf("per person × 20 = %+v", c)
	}
	if res.Limit["flavor-gpu-l4"] != 1 || len(c.Availabilities) != 1 {
		t.Errorf("the private flavour's availability is asked for: %v", res.Limit)
	}
	near(t, "vcpu hours per week", c.VCPUHours.PerWeek, 180*4)
	near(t, "vcpu hours in 30 days", c.VCPUHours.PerWindow, 180*4*30.0/7)
	near(t, "vcpu hours total", c.VCPUHours.Total, 180*4*12)

	// Without the availability the GPU flavour is counted, flagged and not asked for.
	res, err = e.Estimate(in, env(nil), msg.DE)
	if err != nil {
		t.Fatal(err)
	}
	if !hasNote(res.Warnings, "flavor_not_allowed") || res.Limit["flavor-gpu-l4"] != 0 || res.Cloud.Items[1].Allowed {
		t.Errorf("a flavour the budget does not hold is flagged: %+v %v", res.Warnings, res.Limit)
	}
}

func TestCloudUnknownFlavorIsRefused(t *testing.T) {
	e := New(DefaultConfig(), nil)
	_, err := e.Estimate(Input{Cloud: &CloudInput{Flavors: []FlavorCount{{Flavor: "m9.gigantic", Count: 1}}}}, env(nil), msg.DE)
	if err == nil || !strings.Contains(msg.TextOf(err).In(msg.DE), "m1.large") {
		t.Fatalf("an unknown flavour is refused with the possible ones: %v", err)
	}
	_, err = e.Estimate(Input{Cloud: &CloudInput{Flavors: []FlavorCount{{Flavor: "m1.large", Count: 1}}}}, Env{}, msg.DE)
	if err == nil || !strings.Contains(msg.TextOf(err).In(msg.DE), "nicht verbunden") {
		t.Fatalf("without any flavour known the refusal says why: %v", err)
	}
}

func TestWorkspacesCPUAndGPU(t *testing.T) {
	e := New(DefaultConfig(), nil)
	res, err := e.Estimate(Input{Workspaces: &WorkspaceInput{Kind: "cpu", Persons: 20, StorageGBPerPerson: 5, SharedStorageGB: 10, Schedule: Schedule{SessionsPerWeek: 1, HoursPerSession: 3.5, Weeks: 12, Together: true}}}, env(nil), msg.DE)
	if err != nil {
		t.Fatal(err)
	}
	w := res.Workspaces
	// 70 h a week, 300 in 30 days; × 0.5 cores × 1.2 = 180, × 2 GB × 1.2 = 720
	near(t, "person-hours in 30 days", w.PersonHoursWindow, 300)
	if w.VCPUHours != 180 || w.RAMGBHours != 720 || w.StorageGB != 110 {
		t.Errorf("cpu workspaces = %+v", w)
	}
	near(t, "total vcpu hours", w.TotalVCPUHours, 70*12*0.5)
	want := common.ProjectQuota{"ws-vcpu-hours": 180, "ws-ram-gb-hours": 720, "ws-storage-gb": 110, "ws-profile-cpu": 1}
	for k, v := range want {
		if res.Limit[k] != v {
			t.Errorf("limit[%s] = %d, want %d", k, res.Limit[k], v)
		}
	}

	res, err = e.Estimate(Input{Workspaces: &WorkspaceInput{Kind: "gpu", Persons: 10, Schedule: Schedule{SessionsPerWeek: 2, HoursPerSession: 2, Weeks: 8}}}, env(nil), msg.DE)
	if err != nil {
		t.Fatal(err)
	}
	w = res.Workspaces
	// 40 h a week, 30/7 weeks → 171.43 h; GPU all the time × 1.2 = 205.7 → 206; 30 % of 10 at once
	if w.GPUClass != "l4" || w.GPUHours != 206 || w.ConcurrentGPUs != 3 {
		t.Errorf("gpu workspaces = %+v", w)
	}
	if res.Limit["ws-gpu-hours-l4"] != 206 || res.Limit["ws-gpus-l4"] != 3 || res.Limit["ws-profile-gpu"] != 1 {
		t.Errorf("limit = %v", res.Limit)
	}
	if !hasNote(res.Assumptions, "ws_gpu_class") {
		t.Error("the only class is taken as an assumption")
	}

	// CPU + GPU kernel holds the GPU 30 % of the time; an unknown class is a warning and nothing is assigned.
	res, err = e.Estimate(Input{Workspaces: &WorkspaceInput{Kind: "cpu-gpukernel", GPUClass: "h100", Persons: 10, Schedule: Schedule{SessionsPerWeek: 2, HoursPerSession: 2, Weeks: 8, Together: true}}}, env(nil), msg.DE)
	if err != nil {
		t.Fatal(err)
	}
	if res.Workspaces.GPUHours != ceilInt(40*30.0/7*0.3*1.2) || res.Workspaces.ConcurrentGPUs != 10 || !hasNote(res.Warnings, "ws_gpu_class_unknown") || res.Limit["ws-gpus-h100"] != 0 {
		t.Errorf("gpu kernel = %+v, warnings %+v", res.Workspaces, res.Warnings)
	}
}

func TestResourcesMissingFromTheCatalogueAreWarnings(t *testing.T) {
	e := New(DefaultConfig(), nil)
	res, err := e.Estimate(Input{LLM: &LLMInput{Tool: "chat", Persons: 5, Schedule: Schedule{SessionsPerWeek: 1, HoursPerSession: 1, Weeks: 1}}}, Env{Catalog: []common.Resource{{ID: "llm-mtokens"}}}, msg.DE)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Limit["llm-tpm"]; ok || !hasNote(res.Warnings, "unknown_resource") || res.Limit["llm-mtokens"] == 0 {
		t.Errorf("only known ids are proposed: %v %+v", res.Limit, res.Warnings)
	}
}

func TestValidation(t *testing.T) {
	e := New(DefaultConfig(), nil)
	s := Schedule{SessionsPerWeek: 1, HoursPerSession: 2, Weeks: 4}
	bad := map[string]Input{
		"nothing":       {},
		"level":         {Level: "extreme", LLM: &LLMInput{Tool: "chat", Persons: 1, Schedule: s}},
		"tool":          {LLM: &LLMInput{Tool: "telepathy", Persons: 1, Schedule: s}},
		"persons":       {LLM: &LLMInput{Tool: "chat", Persons: 0, Schedule: s}},
		"weeks":         {LLM: &LLMInput{Tool: "chat", Persons: 1, Schedule: Schedule{SessionsPerWeek: 1, HoursPerSession: 2}}},
		"too many h":    {LLM: &LLMInput{Tool: "chat", Persons: 1, Schedule: Schedule{SessionsPerWeek: 21, HoursPerSession: 24, Weeks: 1}}},
		"negative rate": {LLM: &LLMInput{Tool: "chat", Persons: 1, Schedule: s, RequestsPerHour: new(-1.0)}},
		"empty cloud":   {Cloud: &CloudInput{}},
		"count":         {Cloud: &CloudInput{Flavors: []FlavorCount{{Flavor: "m1.small"}}}},
		"participants":  {Cloud: &CloudInput{Flavors: []FlavorCount{{Flavor: "m1.small", Count: 1}}, PerPerson: true}},
		"hours alone":   {Cloud: &CloudInput{Flavors: []FlavorCount{{Flavor: "m1.small", Count: 1}}, HoursPerWeek: 3}},
		"volume":        {Cloud: &CloudInput{Volumes: []VolumeSpec{{SizeGB: 0, Count: 1}}}},
		"ws kind":       {Workspaces: &WorkspaceInput{Kind: "quantum", Persons: 1, Schedule: s}},
		"ws storage":    {Workspaces: &WorkspaceInput{Kind: "cpu", Persons: 1, Schedule: s, StorageGBPerPerson: -1}},
	}
	for name, in := range bad {
		if _, err := e.Estimate(in, env(nil), msg.DE); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAllowedFlavorsAndCache(t *testing.T) {
	got := AllowedFlavors(testFlavors, testGrants, map[string]bool{"flavor-gpu-l4": true})
	names := []string{}
	for _, f := range got {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "m1.small,m1.large,g1.l4" {
		t.Errorf("allowed = %v (public ones, the granted private one, not the private one nobody offers)", names)
	}
	if got[2].Resource != "flavor-gpu-l4" {
		t.Error("an allowed private flavour carries its availability")
	}

	now := time.Unix(0, 0)
	src := &flakySource{flavors: testFlavors[:1]}
	c := &CachedFlavors{Source: src, Fallback: testFlavors[1:2], TTL: time.Minute, now: func() time.Time { return now }}
	src.err = errors.New("not connected")
	if fs, err := c.Flavors(context.Background()); err != nil || fs[0].Name != "m1.large" {
		t.Fatalf("fallback while the source fails: %v %v", fs, err)
	}
	src.err = nil
	if fs, _ := c.Flavors(context.Background()); fs[0].Name != "m1.small" || src.calls != 2 {
		t.Fatalf("the source once it answers: %v", fs)
	}
	if _, _ = c.Flavors(context.Background()); src.calls != 2 {
		t.Error("cached within the TTL")
	}
	now = now.Add(2 * time.Minute)
	src.err = errors.New("gone again")
	if fs, err := c.Flavors(context.Background()); err != nil || fs[0].Name != "m1.small" {
		t.Error("the last answer beats the fallback")
	}
}

type flakySource struct {
	flavors []Flavor
	err     error
	calls   int
}

func (f *flakySource) Flavors(context.Context) ([]Flavor, error) {
	f.calls++
	return f.flavors, f.err
}

func TestGermanDecimals(t *testing.T) {
	got := germanDecimals("20 Personen × 3.5 h = 70; Modell qwen3.8-flash (Profil vom 2026-11-25), 1.25 Spielraum, (0.571 Mio.)")
	want := "20 Personen × 3,5 h = 70; Modell qwen3.8-flash (Profil vom 2026-11-25), 1,25 Spielraum, (0,571 Mio.)"
	if got != want {
		t.Errorf("germanDecimals = %q, want %q", got, want)
	}
}

// Measured figures replace the profile's per figure; what is missing stays the profile's, what the input gives wins; the notes say which is which.
func TestLLMMeasuredProfiles(t *testing.T) {
	e := New(DefaultConfig(), nil)
	from := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	e.SetMeasured(&Measured{From: from, To: from.AddDate(0, 0, 14), MinKeys: 5, Models: map[string]MeasuredModel{
		"code": {
			InputTokensPerRequest: &MeasuredStat{Median: 15000, P90: 40000, Samples: 300, Keys: 12},
			RequestsPerKeyHour:    &MeasuredStat{Median: 6, P90: 20, Samples: 40, Keys: 12},
			PeakTPMPerKey:         &MeasuredStat{Median: 30000, P90: 80000, Samples: 40, Keys: 12},
		},
	}})
	in := Input{Level: LevelHigh, LLM: &LLMInput{Tool: "code", Persons: 20, Schedule: Schedule{SessionsPerWeek: 1, HoursPerSession: 3.5, Weeks: 12, Together: true}}}
	res, err := e.Estimate(in, env(nil), msg.DE)
	if err != nil {
		t.Fatal(err)
	}
	l := res.LLM
	// per request the median also at the high level, requests and peak at their P90
	if l.InputTokensPerRequest != 15000 || l.OutputTokensPerRequest != 200 || l.RequestsPerHour != 20 || l.PeakTPMPerPerson != 80000 {
		t.Errorf("figures = %+v", l)
	}
	want := map[string]string{"input_tokens_per_request": SourceMeasured, "output_tokens_per_request": SourceProfile, "requests_per_hour": SourceMeasured, "peak_tpm_per_person": SourceMeasured}
	for k, v := range want {
		if l.Sources[k] != v {
			t.Errorf("source of %s = %q, want %q", k, l.Sources[k], v)
		}
	}
	if !hasNote(res.Assumptions, "llm_measured") || !hasNote(res.Assumptions, "llm_assumed") {
		t.Fatalf("notes = %+v", res.Assumptions)
	}
	for _, n := range res.Assumptions {
		if n.Code == "llm_measured" && (!strings.Contains(n.Text, "2026-11-11") || !strings.Contains(n.Text, "300 Werte, 12 Schlüssel")) {
			t.Errorf("measured note = %q", n.Text)
		}
	}
	// The input wins over the measurement; another alias is not touched.
	rph := 3.0
	in.LLM.RequestsPerHour = &rph
	res, _ = e.Estimate(in, env(nil), msg.EN)
	if res.LLM.RequestsPerHour != 3 || res.LLM.Sources["requests_per_hour"] != SourceGiven {
		t.Errorf("given = %+v", res.LLM)
	}
	res, _ = e.Estimate(Input{LLM: &LLMInput{Tool: "chat", Persons: 1, Schedule: Schedule{SessionsPerWeek: 1, HoursPerSession: 1, Weeks: 1}}}, env(nil), msg.DE)
	if res.LLM.Sources["requests_per_hour"] != SourceProfile || hasNote(res.Assumptions, "llm_measured") {
		t.Errorf("chat is not measured: %+v", res.LLM.Sources)
	}
	e.SetMeasured(nil)
	if e.Measured() != nil {
		t.Error("cleared")
	}
}
