# Plan: Obergrenzen der Budgets als Katalogeinträge der Anbieter

Stand 10.11.2026. Ziel: Der Baum (`internal/core/model`, `internal/core/tree`) kennt keine konkreten Einstellungen und keinen konkreten Anbieter mehr. Jeder Anbieter meldet Ressourcen und Obergrenzen auf dieselbe Art über seinen Katalog an, dazu seine Mitgliedergrenze über ein schmales Interface. API-Kompatibilität ist nicht nötig (entschieden 10.11.2026): os-mgt und UI gehen gemeinsam raus, der KI-Dienst bleibt unverändert.

## Ausgangslage

Vier Einstellungen des KI-Dienstes sind feste Felder des Knotens, jede mit eigenem Modell, eigener Prüfung, eigenen Anfragefeldern, eigener Historie und eigenem UI-Formular: `ReleaseMax` (CPU/GPU × drei Zeiten), `WorkspaceScratchGB`, `BatchPriority`, `BatchJobs`. Alle folgen derselben Regel: Obergrenze für alles unterhalb des Budgets, nie summiert, nur verschärfbar, es gilt der strengste Wert entlang der Kette. Dazu entscheidet `involvesOpenStack` über die Mitgliedergrenze (32 mit OpenStack, sonst 200).

## Zielbild

### Katalogeintrag `Cap`

Neuer Typ in `internal/common`, getrennt von `Resource`, weil er anders gespeichert und anders gerechnet wird (sonst müsste jede Schleife über Ressourcen ihn ausnehmen). Ein Anbieter-Katalog liefert künftig `Resources()` und `Caps()`.

```go
type Cap struct {
    ID       string   `json:"id"`
    Name     string   `json:"name"`
    Provider string   `json:"provider"`
    Group    string   `json:"group,omitempty"`    // Überschrift in der UI
    Unit     string   `json:"unit,omitempty"`
    Min      int      `json:"min"`                 // erlaubter Bereich eines gesetzten Werts
    Max      int      `json:"max"`
    Unbounded bool    `json:"unbounded,omitempty"` // 0 ist erlaubt und heißt „keine Grenze“, der lockerste Wert
    Choices  []string `json:"choices,omitempty"`   // Auswahl, vom strengsten zum lockersten; gespeichert wird der Index
    Grant    bool     `json:"grant,omitempty"`     // fehlend heißt „aus“ statt „keine Einschränkung von hier“
    Together string   `json:"together,omitempty"`  // Einträge mit derselben Marke sind alle gesetzt oder keiner
    Message  string   `json:"message,omitempty"`   // Erklärung in der UI
}
```

Werte sind `int`. Strenger ist immer der kleinere Wert; 0 bei `Unbounded` zählt als lockerster. Eine Auswahl ist so geordnet, dass auch dort der kleinere Index strenger ist.

### Speicherung am Knoten

- `Node.Caps map[string]int`: nur an Budgets, wie heute (`createBudgetSettings`, Rechte wie `updatePolicy`, keine Genehmigung).
- `Node.InheritedCaps map[string]int`: berechnet, nie gespeichert (ersetzt die vier `Inherited…`).
- `HistoryEntry.CapsFrom/CapsTo map[string]int`: nur die geänderten Einträge.
- Anfrage: `caps` in Create und Update; `null` für einen Eintrag entfernt ihn, wie heute `null` für ein Feld.

### Allgemeine Regeln im Baum (eine Implementierung statt vier)

1. **Auflösen** (`ResolveCaps(chain, catalog)`): je Eintrag der strengste gesetzte Wert entlang der Kette. Für `Grant`-Einträge: Ein Budget ohne Wert, das nicht `InheritsLimit` hat, schaltet aus; Budgets mit `InheritsLimit` werden übersprungen (heute `ResolveBatchJobs`).
2. **Prüfen** (`ValidateCaps(own, inherited, root, catalog)`): bekannte ID; Wert in `Min..Max`, oder 0 bei `Unbounded`, oder gültiger Index bei `Choices`; nicht lockerer als der geerbte Wert; `Grant` nur dort einschaltbar, wo darüber an ist (außer an der Wurzel); `Together` alle oder keiner. Die Fehlertexte nennen den Namen aus dem Katalog.
3. **`InheritsLimit`**: Beim Einschalten verliert das Budget seine eigenen `Grant`-Werte, beim Ausschalten bekommt es die bis dahin geerbten als eigene (heute die Sonderregel in `op_lifecycle.go` für `BatchJobs`, dann allgemein).
4. **Unbekannte IDs** (Eintrag aus dem Katalog verschwunden): beim Lesen ignoriert, beim Schreiben abgelehnt, beim Ändern eines Budgets nicht mitgeschleppt.

### Die vier Einstellungen im Katalog des KI-Dienstes

In `internal/provider/aiaas` als feste Liste (sie hängen an der Semantik des KI-Dienstes, nicht an der Bereitstellung):

| ID | Min–Max | Besonderheit |
|---|---|---|
| `release-cpu-idle-open` | 1–1440 min | |
| `release-cpu-idle-closed` | 1–1440 min | |
| `release-cpu-max-hours` | 1–720 h | nicht `Unbounded` |
| `release-gpu-idle-open` | 1–1440 min | |
| `release-gpu-idle-closed` | 1–1440 min | |
| `release-gpu-max-hours` | 1–720 h | `Unbounded` |
| `workspace-scratch-gb` | 1–200 GB | |
| `batch-priority` | Auswahl `preemptible`, `guaranteed` | |
| `batch-jobs-per-person` | 1–… | `Grant`, `Together: batch-jobs` |
| `batch-max-hours` | 1–24 h | `Grant`, `Together: batch-jobs` |

`mapping.go` übersetzt die aufgelösten Werte in das bisherige Format an den KI-Dienst (`Release`, `WorkspaceScratchGB`, `BatchPriority`, `BatchJobs`). Der Draht zum KI-Dienst ändert sich nicht.

### Mitgliedergrenze je Anbieter

```go
type ProviderRules interface {
    Provider() string
    MaxMembers() int
}
```

- Der Baum ermittelt allgemein, welche Anbieter ein Projekt betrifft: Bindung des Anbieters oder mehr als null von einer seiner Ressourcen (dieselbe Logik wie `ProviderSync.Leaves`, eine gemeinsame Funktion). Grenze = Minimum der `MaxMembers()` dieser Anbieter; betrifft das Projekt keinen, gilt eine allgemeine Grenze des Baums (200).
- Die Prüfung beim Wechsel (`op_allocations.go:138`, `op_lifecycle.go:667`) wird zu „die Grenze sinkt durch die Änderung“.
- `API_MAX_AUTHORIZED_USERS` wandert in die Konfiguration des OpenStack-Anbieters, der KI-Dienst bringt 200 mit. `involvesOpenStack`, `maxAuthorizedUsersOther`, `SetMaxAuthorizedUsersOther` und die beiden Konstanten in `common` entfallen.
- Geprüft 10.11.2026: Der Reconciler gibt jedem Import die OpenStack-Bindung (`imports.go`), und auf Staging und Prod gibt es keine importierten Projekte. Die Sonderregel für `StatusImported` entfällt daher.

## Schritte

1. **Äquivalenztest zuerst.** Ein Test in `internal/provider/aiaas`, der für eine Reihe von Budgetketten (heutige Testfälle aus `release_test.go`, `scratch_test.go`, `batch_jobs_test.go`, `batch_priority_test.go`, `rule_batch_*`, `rule_workspace_scratch_test.go`) das Ergebnis von `ReleaseFor`/`ScratchFor`/`BatchPriorityFor`/`BatchJobsFor` festhält. Er muss nach dem Umbau mit denselben Erwartungen grün bleiben.
2. **`common.Cap` und Katalog.** Typ, `ValidateCaps` des Katalogs beim Start (eindeutige IDs, `Min ≤ Max`, `Choices` nicht leer wo gesetzt, `Together` nur innerhalb eines Anbieters), `Caps()` im Katalog-Interface, `CombinedCatalog` erweitert.
3. **Modell und Baum.** `Node.Caps`, `InheritedCaps`, `CapsFrom/To`; `ResolveCaps`/`ValidateCaps` mit Tabellentests gegen einen Testkatalog; `op_requests.go`, `op_lifecycle.go`, `calc_decorate.go`, `nodestore/postgres.go` umstellen. Die vier Felder, ihre `Clear…`-Flags und die Dateien `release.go`, `scratch.go`, `batch_jobs.go`, `batch_priority.go` entfallen.
4. **Katalog des KI-Dienstes.** Die zehn Einträge in `internal/provider/aiaas`, `mapping.go` und `sync.go` auf `ResolveCaps` umstellen; Äquivalenztest aus Schritt 1 grün.
5. **Datenmigration.** Beim Start im Postgres-Store, nach dem Muster von `postgres_migrate_enddates.go`: in `data` jedes Knotens `release_max`, `workspace_scratch_gb`, `batch_priority`, `batch_jobs` nach `caps` übertragen, ebenso die Felder `…_from/_to` jedes Historieneintrags nach `caps_from/caps_to`; idempotent, in einer Transaktion, mit Test auf echten Altdaten (Abzug von Staging). Vor dem Rollout auf Prod ein Backup des CNPG-Clusters.
6. **`/v1/config` und Swagger.** `caps` neben `resources` ausliefern; Swagger neu erzeugen; MCP-Beschreibungen prüfen.
7. **Mitgliedergrenze.** `ProviderRules`, gemeinsame Funktion „betroffene Anbieter“, Konfiguration verschieben, Tests aus `rule_members_cap_by_provider_test.go` auf Anbieter-Grenzen umstellen.
8. **UI.** Eine allgemeine Komponente `component-caps.jsx` statt `component-release-max`, `-workspace-scratch`, `-batch-priority`, `-batch-jobs` und ihrer `util-…`-Dateien: Einträge nach `group`, `together` als ein Block mit Schalter, `choices` als Auswahl, `unbounded` als „unbegrenzt“, geerbter Wert als Platzhalter. `modal-budget-form.jsx` und die Historie in `modal-inspect.jsx` auf `caps` umstellen; die Render-Tests anpassen. `web/gpu/workspace-settings.jsx` bleibt (liest beim KI-Dienst).
9. **Rollout.** os-mgt und UI zusammen, Staging mit Migration und Sichtprüfung der Budget-Formulare und der Werte beim KI-Dienst (unverändert gegenüber vorher), dann Prod.

### Rolle der Mitglieder

`openstack_role` heißt `role` (entschieden 10.11.2026). `common.Roles` (`member`, `reader`) sind die Zugriffsstufen des Baums; jeder Anbieter übersetzt sie: OpenStack ausdrücklich in `osclient.KeystoneRole` (Eigentümer: `osclient.OwnerKeystoneRole`), der KI-Dienst `reader` in nur lesend. Die Migration benennt das Feld in `authorized_users` und `pending.authorized_users` um.

## Umgesetzt (10.11.2026)

Alle Schritte. Abweichungen vom Plan: `Cap` hat zusätzlich `Area` (der Reiter der UI); ein Update führt `caps` zusammen (ein Wert setzt, `null` entfernt, nicht genannte bleiben, `"caps": null` entfernt alle). Die Migration ist beim Start fatal, weil nichts die früheren Felder mehr liest; geprüft gegen eine Kopie der Staging-Daten (`TestMigrateCapsPostgres` mit `OSMGT_TEST_CAPS_DSN`) und mit den Formen der Prod-Daten (`TestMigrateNodeData`). `TestCapsWireEquivalence` hält fest, dass der KI-Dienst dieselben Werte bekommt wie vorher.

## Was danach im Baum noch konkret ist

Importe (`StatusImported`, Bootstrap-Knoten „Unassigned OpenStack Imports“), die Verbrauchsbücher nach OpenStack-Art und die feste Liste der Bereiche. Bewusst nicht Teil dieses Plans.
