package usage

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// What the daily rows add up to over a period: for one project, for a budget's
// subtree, or for everything. Reading only — nothing here writes a row.

// Prices are list prices of a public cloud per unit, in euro, to put a value on
// what was used. Only the root admins' evaluation shows it: on a single project a
// euro amount invites the wrong conclusions.
type Prices struct {
	VCPUHour     float64 `json:"vcpu_hour"`
	RAMGBHour    float64 `json:"ram_gb_hour"`
	StorageGBDay float64 `json:"storage_gb_day"`
}

// Valid reports whether any price is set; without one there is no value.
func (p *Prices) Valid() bool {
	return p != nil && (p.VCPUHour > 0 || p.RAMGBHour > 0 || p.StorageGBDay > 0)
}

// Totals sums days. Reserved amounts are counted the way used ones are — core
// hours, GB hours, GB days — so that dividing one by the other is utilisation.
type Totals struct {
	ServerHours float64 `json:"server_hours"`
	VCPUHours   float64 `json:"vcpu_hours"`
	RAMGBHours  float64 `json:"ram_gb_hours"`
	// StorageGBDays sums the daily storage samples; SampledDays counts the days
	// that have one (backfilled days do not).
	StorageGBDays float64 `json:"storage_gb_days"`
	SampledDays   int     `json:"sampled_days"`
	// PublicIPv4Days sums the daily counts of public IPv4 addresses;
	// IPv4SampledDays counts the days that have one.
	PublicIPv4Days  float64 `json:"public_ipv4_days"`
	IPv4SampledDays int     `json:"ipv4_sampled_days"`

	ReservedCoreHours  float64 `json:"reserved_core_hours"`
	ReservedRAMGBHours float64 `json:"reserved_ram_gb_hours"`
	// ReservedStorageGBDays counts sampled days only, to compare like with like.
	ReservedStorageGBDays float64 `json:"reserved_storage_gb_days"`
}

// Utilization is used divided by reserved, per resource; absent where nothing
// was reserved.
type Utilization struct {
	Cores   *float64 `json:"cores,omitempty"`
	RAM     *float64 `json:"ram,omitempty"`
	Storage *float64 `json:"storage,omitempty"`
}

// DayTotals is one day of the period, summed over the projects in it.
type DayTotals struct {
	Day      string `json:"day"`
	Projects int    `json:"projects"`
	Totals
}

// ProjectTotals is one project over the period, named as it was on the last day
// that has a row for it.
type ProjectTotals struct {
	NodeID      string      `json:"node_id"`
	ProjectName string      `json:"project_name"`
	Owner       string      `json:"owner"`
	Status      string      `json:"status"`
	BudgetID    string      `json:"budget_id"`
	BudgetPath  []PathEntry `json:"budget_path"`
	// Attributes are the attribute groups that applied over these days. A
	// project whose attributes changed within the period is reported once per
	// set of attributes, so each part can be billed to where it belongs.
	Attributes tree.Attributes `json:"attributes,omitempty"`
	People     int             `json:"people"`
	Days       int             `json:"days"`
	// LastActive is the last day a server ran in it, empty for none.
	LastActive  string      `json:"last_active,omitempty"`
	Utilization Utilization `json:"utilization"`
	ValueEUR    *float64    `json:"value_eur,omitempty"`
	Totals
}

// Report is the consumption of a set of projects over [From, To], both days
// included.
type Report struct {
	From string `json:"from"`
	To   string `json:"to"`
	// BackfilledDays counts the days collected after the fact: their snapshot
	// is the tree as it was at collection time, and they have no storage sample.
	BackfilledDays int             `json:"backfilled_days"`
	Utilization    Utilization     `json:"utilization"`
	ValueEUR       *float64        `json:"value_eur,omitempty"`
	Prices         *Prices         `json:"prices,omitempty"`
	Days           []DayTotals     `json:"days"`
	Projects       []ProjectTotals `json:"projects"`
	Totals
}

// Mapping names the catalogue resources that reserve what Nova and Cinder
// measure, and the factor from the catalogue's unit to GB for RAM.
type Mapping struct {
	Cores, RAM, Storage string
	RAMToGB             float64
}

// MappingFrom finds them by the OpenStack quota field they map to.
func MappingFrom(catalog []common.ManagedProject) Mapping {
	m := Mapping{RAMToGB: 1.0 / 1024}
	for _, r := range catalog {
		switch r.OSQuotaField {
		case "cores":
			m.Cores = r.ID
		case "ram":
			m.RAM = r.ID
			// Nova counts RAM in MB; a catalogue in GB carries a multiplier.
			mult := r.OSMultiplier
			if mult <= 0 {
				mult = 1
			}
			m.RAMToGB = float64(mult) / 1024
		case "gigabytes":
			m.Storage = r.ID
		}
	}
	return m
}

func (t *Totals) add(d Day, m Mapping) {
	t.ServerHours += d.ServerHours
	t.VCPUHours += d.VCPUHours
	t.RAMGBHours += d.RAMGBHours
	t.ReservedCoreHours += float64(d.Reserved[m.Cores]) * 24
	t.ReservedRAMGBHours += float64(d.Reserved[m.RAM]) * m.RAMToGB * 24
	if !d.Backfilled {
		t.StorageGBDays += d.StorageGB
		t.SampledDays++
		t.ReservedStorageGBDays += float64(d.Reserved[m.Storage])
	}
	if d.PublicIPv4 != nil {
		t.PublicIPv4Days += float64(*d.PublicIPv4)
		t.IPv4SampledDays++
	}
}

func (t *Totals) merge(o Totals) {
	t.ServerHours += o.ServerHours
	t.VCPUHours += o.VCPUHours
	t.RAMGBHours += o.RAMGBHours
	t.StorageGBDays += o.StorageGBDays
	t.SampledDays += o.SampledDays
	t.PublicIPv4Days += o.PublicIPv4Days
	t.IPv4SampledDays += o.IPv4SampledDays
	t.ReservedCoreHours += o.ReservedCoreHours
	t.ReservedRAMGBHours += o.ReservedRAMGBHours
	t.ReservedStorageGBDays += o.ReservedStorageGBDays
}

func (t Totals) utilization() Utilization {
	ratio := func(used, reserved float64) *float64 {
		if reserved <= 0 {
			return nil
		}
		r := used / reserved
		return &r
	}
	return Utilization{
		Cores:   ratio(t.VCPUHours, t.ReservedCoreHours),
		RAM:     ratio(t.RAMGBHours, t.ReservedRAMGBHours),
		Storage: ratio(t.StorageGBDays, t.ReservedStorageGBDays),
	}
}

func (t Totals) value(p *Prices) *float64 {
	if !p.Valid() {
		return nil
	}
	v := t.VCPUHours*p.VCPUHour + t.RAMGBHours*p.RAMGBHour + t.StorageGBDays*p.StorageGBDay
	return &v
}

// BuildReport sums rows over [from, to]. Prices are only passed for the root
// admins' evaluation; nil leaves every value out.
func BuildReport(rows []Day, from, to time.Time, m Mapping, prices *Prices) Report {
	r := Report{From: dayOf(from).Format(time.DateOnly), To: dayOf(to).Format(time.DateOnly), Days: []DayTotals{}, Projects: []ProjectTotals{}}
	if prices.Valid() {
		r.Prices = prices
	}
	days := map[string]*DayTotals{}
	backfilled := map[string]bool{}
	projects := map[string]*ProjectTotals{}
	for _, d := range rows {
		key := d.Day.Format(time.DateOnly)
		dt := days[key]
		if dt == nil {
			dt = &DayTotals{Day: key}
			days[key] = dt
		}
		dt.Projects++
		dt.add(d, m)
		if d.Backfilled {
			backfilled[key] = true
		}

		pk := projectKey(d)
		p := projects[pk]
		if p == nil {
			p = &ProjectTotals{NodeID: d.NodeID, Attributes: d.Attributes}
			projects[pk] = p
		}
		// Rows come oldest first, so the last one names the project.
		p.ProjectName, p.Owner, p.Status = d.ProjectName, d.Owner, d.Status
		p.BudgetID, p.BudgetPath, p.People = d.BudgetID, d.BudgetPath, d.People
		p.Days++
		p.add(d, m)
		if d.ServerHours > 0 {
			p.LastActive = key
		}
	}
	for _, dt := range days {
		r.Days = append(r.Days, *dt)
		r.merge(dt.Totals)
	}
	slices.SortFunc(r.Days, func(a, b DayTotals) int {
		if a.Day < b.Day {
			return -1
		}
		if a.Day > b.Day {
			return 1
		}
		return 0
	})
	r.BackfilledDays = len(backfilled)
	for _, p := range projects {
		p.Utilization = p.utilization()
		p.ValueEUR = p.value(prices)
		r.Projects = append(r.Projects, *p)
	}
	// The biggest consumers first: that is what a report is read for.
	slices.SortFunc(r.Projects, func(a, b ProjectTotals) int {
		if a.VCPUHours != b.VCPUHours {
			if a.VCPUHours > b.VCPUHours {
				return -1
			}
			return 1
		}
		if a.NodeID != b.NodeID {
			if a.NodeID < b.NodeID {
				return -1
			}
			return 1
		}
		return strings.Compare(attributesKey(a.Attributes), attributesKey(b.Attributes))
	})
	r.Utilization = r.utilization()
	r.ValueEUR = r.value(prices)
	return r
}

// projectKey separates a project's rows by the attributes that applied, so a
// change of cost centre within a period splits it rather than billing every
// day to the last one.
func projectKey(d Day) string {
	return d.NodeID + "\x00" + attributesKey(d.Attributes)
}

// attributesKey is a canonical form: encoding/json sorts map keys.
func attributesKey(a tree.Attributes) string {
	if len(a) == 0 {
		return ""
	}
	b, _ := json.Marshal(a)
	return string(b)
}
