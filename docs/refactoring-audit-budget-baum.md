# Refactoring-Audit: Budgetbaum (os-mgt-api)

Stand: 10.11.2026, Commit `a7d72d3` (release 0.8.75). Umfang: `internal/core/tree`, `internal/core/model`, `internal/core/nodestore` (soweit Abfragen des Baums), die Knoten-Handler in `internal/webserver/api_nodes.go`, die MCP-Werkzeuge in `internal/webserver/mcp.go` sowie die Stellen in `internal/provider/aiaas` und `internal/provider/openstack/reconciler`, die Enddaten oder Limits des Baums lesen. Es wurde kein Code geändert; Zeilenangaben beziehen sich auf diesen Commit.

## Kurzfazit

Die Codebasis ist für eine KI-generierte Codebasis ungewöhnlich diszipliniert: Die Schichten (`webserver → tree.Service → nodestore.Store`) werden eingehalten, Regeln sind nach Dateipräfixen geordnet, und zentrale Entscheidungen (Abrechnung, `chargedStatuses`, `checkLeafLimit`/`checkBudgetLimit`) haben bewusst genau einen Ort. Der Duplikationsgrad ist insgesamt mäßig (geschätzt 5–8 % der rund 6.500 Produktionszeilen im Umfang), konzentriert sich aber genau dort, wo der Fehler von 0.8.75 entstand: Enddaten sind ein `*string`, und Parsen, Normalisieren, Vergleichen, Gleichheit, Sortieren und Anzeigen sind an mindestens sieben Stellen mit unterschiedlicher Semantik umgesetzt. Ähnlich, aber weniger gefährlich, sind die Prädikate über Limits (`> 0 || == -1`, „Budget hält eine Verfügbarkeit“, „passt unter das Limit“) mehrfach von Hand geschrieben. Die wichtigste Maßnahme ist daher ein eigener Typ bzw. ein eigenes Modul für Enddaten mit einer einzigen Vergleichssemantik, gefolgt von einer Normalisierung des Bestands.

## Bestehende Konventionen (aus ARCHITECTURE.md und Code)

- Alle Autorisierung liegt im Service, Handler greifen nie auf den Store zu (ARCHITECTURE.md:227).
- Kapazität wird auf jedem Pfad geprüft, der Verbrauch aktiv macht; jede Limit-Setzung läuft über `checkLeafLimit` bzw. `checkBudgetLimit` (rule_limits.go:386–446).
- Validierungsfehler tragen keinen Sentinel und werden zu 400; `common.ErrForbidden/ErrNotFound/ErrConflict/ErrInternal` steuern 403/404/409/500 (webserver/errors.go:11–30).
- Werte, die entlang der Kette „strenger werden“ (Laufzeit, Release-Maxima, Scratch, Batch-Priorität, Batch-Jobs, Ende), werden per `Resolve*(chain)` bzw. `chain*` aufgelöst und dürfen unten nur verschärft werden.
- Kommentare sind ausführlich und begründen Entscheidungen; das sollte bei jedem Umbau erhalten bleiben.

## Top 10 Maßnahmen

### 1. Enddaten als eigener Wert mit einer Semantik (höchste Priorität)

Problem: `TerminationDate` ist `*string` (model.go:472). Jede Stelle, die damit rechnet, parst selbst, und zwar unterschiedlich. Genau diese Streuung war die Ursache des Fehlers vom 09./10.11.2026 (Commit `bb5367c`), und sie besteht nach dem Fix an anderen Stellen fort.

Fundstellen und Abweichungen:

- `parseEnd` (rule_terms.go:15–20): RFC 3339, dann `DateOnly`; Ergebnis in der Zone des Strings.
- `normalizeEnd` (rule_terms.go:23–37): parst ein zweites Mal selbst statt `parseEnd` zu nutzen; RFC 3339 bleibt **wörtlich** stehen, also auch mit Offset (`+02:00`); ein reines Datum wird zu `…T00:00:00.000Z`.
- `termEnd` (rule_terms.go:112–115) schreibt das Laufzeitende als `time.RFC3339` ohne Millisekunden und mit `23:59:59Z`. Damit gibt es im Bestand mindestens drei Schreibweisen: `2027-06-30`, `2027-06-30T00:00:00.000Z`, `2027-06-30T23:59:59Z` (plus beliebige Offsets).
- `dateOnly` (rule_terms.go:169–174) für Fehlermeldungen kennt nur RFC 3339 und formatiert ohne `.UTC()`, zeigt also den Tag in der Zone des Strings.
- `sameEnd` (op_lifecycle.go:1038–1043) vergleicht Strings mit `==`; zwei Schreibweisen desselben Zeitpunkts gelten als verschieden (Folge: ein überflüssiger Verlaufseintrag in `ApproveNode`, op_lifecycle.go:990–993).
- Sortierung nach `termination_date` (op_children.go:404–408) vergleicht Strings lexikografisch; mit Offsets oder gemischten Schreibweisen ist die Reihenfolge falsch.
- `parseDay` im KI-Provider (aiaas/mapping.go:153–160) prüft zuerst `DateOnly`, dann RFC 3339 und wandelt nach UTC; `EndOf` schneidet auf den UTC-Tag (aiaas/sync.go:187–208). Für `2027-06-30T01:00:00+02:00` ergibt das den 29.06., `dateOnly` im Baum zeigt den 30.06.
- Der Reconciler schreibt den String wörtlich als Tag (reconciler/tags.go:34–35), d. h. die OpenStack-Tags tragen ebenfalls drei Schreibweisen.
- Fehlersemantik bei unlesbaren Daten ist uneinheitlich: `endsNoLater` antwortet `false` (fail-closed, „Verlängerung“, rule_terms.go:39–55), `chainEnd` überspringt ein unlesbares Budgetende, d. h. es begrenzt nichts (fail-open, rule_terms.go:57–76), `shortenEnd` kürzt ein unlesbares Ende (rule_terms.go:261).
- Die Normalisierung am Eingang muss an jedem Einstieg einzeln aufgerufen werden: op_lifecycle.go:30, :382, :649 und op_move.go:244. Ein neuer Schreibpfad, der sie vergisst, öffnet die Lücke wieder.
- Die MCP-Beschreibung verlangt weiterhin „RFC3339“ (mcp.go:288), obwohl der Service jetzt auch reine Daten annimmt.

Vorgeschlagene Abstraktion: ein kleines Paket `internal/core/enddate` (oder ein Typ in `model`), das die einzige Stelle ist, die das Format kennt. Skizze:

```go
type End struct{ t time.Time }               // immer UTC
func Parse(s string) (End, error)            // RFC 3339 oder 2006-01-02, sonst Fehler
func Normalize(s *string) (*string, error)   // Parse + Canonical; nil bleibt nil
func (e End) Canonical() string              // eine Schreibweise, z. B. 2006-01-02T15:04:05.000Z in UTC
func (e End) Day() string                    // für Meldungen und Tags, immer UTC
func Compare(a, b *string) int               // nil = „kein Ende“ = +∞; unlesbar: definiert, siehe offene Fragen
func Equal(a, b *string) bool
```

`parseEnd`, `normalizeEnd`, `dateOnly`, `sameEnd`, `endsNoLater`, `aiaas.parseDay` und der Sortierschlüssel in `op_children.go` werden zu Aufrufen davon. Langfristig kann `Node.TerminationDate` den Typ selbst tragen (mit `UnmarshalJSON`, das normalisiert); dann entfallen die vier manuellen `normalizeEnd`-Aufrufe ganz. Das ist aber ein Schemawechsel und gehört in einen eigenen Schritt.

Nutzen: hoch. Etwa 60 Zeilen werden zu rund 40, wichtiger ist die eine Vergleichssemantik für Baum, KI-Provider, Reconciler und Sortierung. Risiko: mittel; die Vergleiche entscheiden über Auto-Genehmigung von Verlängerungen und über Kürzungen ganzer Teilbäume. Aufwand: M.

Nötige Tests vor dem Umbau: Tabellentest für `parseEnd`/`normalizeEnd`/`dateOnly`/`sameEnd` mit `2027-06-30`, `…T00:00:00Z`, `…T00:00:00.000Z`, `…T23:59:59Z`, `…T01:00:00+02:00`, `30.06.2027` und Leerraum (bisher gibt es nur den Szenariotest `TestChange_BareDateIsTheSameEnd`, rule_autoapprove_changes_test.go:252–271, und keinen einzigen Test mit Offset; in den Tests kommen nur die Schreibweisen `D`, `DT00:00:00Z`, `DT00:00:00.000Z`, `DT10:00:00Z`, `DT23:30:00Z` vor); Sortiertest `ChildSortEnd` mit gemischten Schreibweisen; `aiaas.EndOf` mit Offset; `leafChangeDecision` mit gleichem Tag in anderer Schreibweise (Mitgliederänderung bleibt direkt).

### 2. Gespeicherte Enddaten einmalig normalisieren

Problem: Der Fix in 0.8.75 normalisiert nur neue Eingaben. Bereits gespeicherte reine Daten (der Anlass, `ma-t-inf-23iki1-datenanalyse-r`) und Daten mit Offset bleiben im Bestand, in `Pending.TerminationDate` und in den Verlaufsfeldern `termination_date_from/to` (model.go:305–306). Solange das so ist, muss jeder Leser alle Schreibweisen können, also bleibt Maßnahme 1 die einzige Verteidigung.

Fundstellen: Die Normalisierung erfolgt nur bei `CreateNode`, `UpdateNode`, `RequestChange`, `PromoteNode` (s. o.); `ProviderSync`, `Bootstrap` und Importe schreiben kein Ende, der Dev-Seed schreibt `time.RFC3339` (dev/seed.go:33). Ein Muster für eine einmalige Migration beim Start existiert bereits (`renameNodeFields`, nodestore/postgres_migrate_fields.go; ARCHITECTURE.md:131).

Vorgeschlagene Abstraktion: eine Startmigration `normalizeEndDates` nach dem Muster von `renameNodeFields`, die `termination_date` und `pending.termination_date` aller Knoten mit `enddate.Normalize` umschreibt (Verlauf bleibt unverändert, er ist ein Protokoll). Danach wird ein Reconciler-Tick die OpenStack-Tags von selbst angleichen (tags.go schreibt nur bei Änderung).

Nutzen: hoch (entfernt die Altlast, nach der jede Prüfung fragen muss). Risiko: mittel (Schreibvorgang auf allen Knoten; idempotent gestalten, vorher Backup). Aufwand: S–M.

Nötige Tests: Migrationstest analog `postgres_migrate_legacy_test.go` mit allen Schreibweisen, Idempotenz (zweiter Lauf schreibt nichts), Knoten ohne Ende bleiben ohne Ende.

### 3. Eine Funktion für „welches Ende darf dieser Knoten haben“ plus injizierbare Uhr

Problem: Die Fallunterscheidung „Projekt: `fitProjectEnd` mit Laufzeit, Budget: `fitEnd(chainEnd)`“ ist an fünf Stellen ausgeschrieben, jeweils mit `time.Now()` direkt im Aufruf.

Fundstellen: `CreateNode` op_lifecycle.go:166–172; `RequestChange` op_lifecycle.go:694–699; `ApproveNode` op_lifecycle.go:958–961 (gleiche Logik als `projectBound`); `ReparentNode` op_move.go:104–111; `PromoteNode` op_move.go:281 (nur Projekt). `UpdateNode` op_lifecycle.go:537–546 ist der Budget-Fall mit zusätzlicher Clear-Regel. `time.Now()` steht im Baum an sieben Stellen (op_lifecycle.go:169, :197, :635, :696, :960; op_move.go:104, :281), während der KI-Provider eine injizierbare `now()` hat (aiaas/sync.go:155).

Vorgeschlagene Abstraktion:

```go
func (s *Service) endBound(n *model.Node, chain []model.Node) (bound *string, term *int)   // Projekt: projectBound, Budget: chainEnd
func (s *Service) fitNodeEnd(n *model.Node, end *string, chain []model.Node) (*string, error)
func (s *Service) now() time.Time                                                          // Feld im Service, in Tests setzbar
```

Nutzen: mittel (rund 25 Zeilen, vor allem eine Stelle für die Regel „nichts überlebt sein Budget“). Mit der Uhr werden die Laufzeittests deterministisch (heute rechnen sie relativ zu `time.Now()`). Risiko: niedrig bis mittel. Aufwand: S.

Nötige Tests: Die vorhandenen `rule_terms_lifetime_test.go` und `rule_terms_max_term_test.go` decken die fünf Pfade weitgehend ab; zu ergänzen ist ein Test für `PromoteNode` mit Laufzeitgrenze und ein Test um Mitternacht UTC mit fester Uhr.

### 4. Prädikate über Limits zentralisieren

Problem: Dieselben Fragen an ein `common.ProjectQuota` sind mehrfach von Hand ausgeschrieben, teils mit feinen Abweichungen.

Fundstellen:

- „hält etwas von dieser Ressource“ (`v > 0 || v == UnlimitedQuota`): calc_decorate.go:342, op_retire.go:145, rule_members.go:182, sync_provider.go:72, op_children.go:251 (fünfmal).
- „Budget gewährt eine Verfügbarkeit“ (Wurzel oder `1` oder `-1`): rule_limits.go:305–307, rule_limits.go:375–379, op_allocations.go:220 (dreimal, jeweils mit eigener Wurzel-Ausnahme). Dagegen prüfen `checkAvailabilityWithdrawal` (rule_limits.go:31) und `descendantsHolding` (rule_limits.go:79, :83) nur `== 1`.
- „passt unter das Limit“ (mit `-1` als unbegrenzt): `shortfall` und `quotaFits` (calc_rollup.go:380–398, `quotaFits` ist `!shortfall`), dazu von Hand in `checkBudgetLimit` (rule_limits.go:414–418).
- „wächst“: `limitShrinks` (rule_limits.go:487–500) unterscheidet Verfügbarkeiten und Mengen, `SetAllocation` berechnet `raises` selbst und ohne diese Unterscheidung (op_allocations.go:85–90).
- „Abzug vor Addition“: identische Schleife in `autoApprovable` (rule_autoapprove.go:41–43) und `checkCapacity` (rule_limits.go:131–133).

Vorgeschlagene Abstraktion: Methoden in `common` bzw. kleine Funktionen im Baum:

```go
func (q ProjectQuota) Holds(id string) bool                         // v > 0 || v == UnlimitedQuota
func (s *Service) grantsAvailability(b *model.Node, id string) bool // Wurzel oder Holds
func shortfall(need, limit ProjectQuota, ids []string) (string, bool) // quotaFits entfällt
func (s *Service) grows(current, next ProjectQuota) bool             // = !limitShrinks, auch für Zuweisungen
```

Nutzen: mittel (rund 40 Zeilen, vor allem eine Definition von „hält“ für Anzeige, Mitgliederobergrenze, Provider-Zuständigkeit und Löschbarkeit). Risiko: niedrig, aber die Wurzel-Ausnahme und die `== 1`-Stellen müssen bewusst entschieden werden (siehe offene Fragen). Aufwand: S.

Nötige Tests: Tabellentest für `Holds` (0, 1, 5, -1, fehlender Schlüssel); `SetAllocation` mit gleichbleibender Verfügbarkeit und kleinerer Menge (darf vom Halter ohne Grund kommen); bestehende `rule_availability_test.go` und `op_allocations_test.go` müssen grün bleiben.

### 5. Entlang der Kette vererbte Budget-Einstellungen: Auflösung nur noch über `model.Resolve*`

Problem: Für fünf „nur verschärfbare“ Budget-Einstellungen gibt es je ein `Stricter*`, ein `Resolve*` und ein `Validate*` in `model`, dazu für die Laufzeit ein eigenes Paar im Baum, und in der Antwort-Dekoration noch einmal je eine eigene Auflösung.

Fundstellen: `model.StricterScratch`/`ResolveWorkspaceScratchGB` (scratch.go:13–31), `StricterBatchPriority`/`ResolveBatchPriority` (batch_priority.go:17–35), `StricterRelease`/`ResolveReleaseMax` (release.go:103–121), `ResolveBatchJobs` (batch_jobs.go:33–50); im Baum `chainMaxTerm`/`fitMaxTerm` (rule_terms.go:97–105, :152–166), die dieselbe „kleinstes `*int`“-Logik wie `StricterScratch` haben. In `calc_decorate.go:98–146` lösen `inheritedReleaseMax`, `inheritedScratch` und `inheritedBatchPriority` die Kette erneut selbst auf, `inheritedBatchJobs` baut dafür Schein-Knoten und ruft `model.ResolveBatchJobs`; `ancestry` hält pro Einstellung eine eigene Map (calc_decorate.go:152–170).

Vorgeschlagene Abstraktion (klein, Rule of three erfüllt): `ancestry` speichert pro geladener ID einen schlanken `model.Node` (nur die Policy-Felder) und bietet `func (a ancestry) budgetsAbove(n model.Node) []model.Node`. Die vier `inherited*` werden zu Einzeilern über die vorhandenen `model.Resolve*`. `chainMaxTerm` wandert als `model.ResolveMaxProjectTermDays` zu den anderen und nutzt dieselbe Min-Hilfe. Eine generische Beschreibungstabelle für alle Einstellungen (Name, gesetzt?, löschen, validieren, anwenden) wäre möglich, wird aber nicht empfohlen: Die Einstellungen unterscheiden sich in Details (Batch-Jobs folgen `InheritsLimit`, Release-Maxima haben kein Clear-Flag), und die Tabelle wäre schwerer zu lesen als der heutige Code.

Nutzen: mittel (rund 50 Zeilen in `calc_decorate.go`, eine Auflösung statt zwei pro Einstellung). Risiko: niedrig. Aufwand: S.

Nötige Tests: Antworttest, dass `inherited_*`-Felder für Projekt, Unterbudget und Wurzel dieselben Werte liefern wie `model.Resolve*` über `parentChainNodes` (eine Eigenschaft, die heute nur indirekt getestet ist).

### 6. Ein Teilbaum-Walker statt fünf BFS-Schleifen

Problem: Die Breitensuche über Budgets ist fünfmal ausgeschrieben; zwei davon sind nahezu identisch.

Fundstellen: `shortenSubtreeEnds` (rule_terms.go:181–208) und `shortenSubtreeTerms` (rule_terms.go:215–243) unterscheiden sich nur in der Mutation pro Kind; dazu `buildSubtreeParentMap` (calc_rollup.go:17–49), `undelegatedBudgetIDs` (rule_authz.go:99–129) und `checkInheritingMove` (op_move.go:141–170).

Vorgeschlagene Abstraktion: zuerst nur die zwei Kürzungsfunktionen zusammenlegen:

```go
func (s *Service) mutateLiveSubtree(ctx context.Context, parentIDs []string, what string, mutate func(child model.Node) func(*model.Node) error) (int, error)
```

`shortenSubtreeEnds` und `shortenSubtreeTerms` werden zu wenigen Zeilen. Ein allgemeiner `forEachBudgetLevel(ctx, roots, visit)` für die übrigen drei ist möglich, lohnt sich aber erst, wenn ein weiterer Walker hinzukommt; die drei haben unterschiedliche Abbruchregeln (gesehen-Menge, Delegation, Vererbung).

Nutzen: mittel (rund 30 Zeilen; eine Stelle für „lebende Knoten, Importe überspringen“). Risiko: niedrig bis mittel (läuft unter `approvalMu`, schreibt auf viele Knoten). Aufwand: S.

Nötige Tests: Ende eines Budgets vorziehen mit Unterbudget, Projekt, wartender Anfrage und Vorschlag (`rule_terms_lifetime_test.go`); Laufzeit senken mit Unterbudget, das schon kürzer ist (`term_shortened` nicht doppelt).

### 7. Verwaltungsrechte auf der bereits geladenen Kette prüfen

Problem: `managesNode` und `managesParentChain` laden die Kette über `nodeChain` jedes Mal neu, mit einem `GetNode` pro Ebene (rule_authz.go:13–33, :38–68), auch wenn die Operation die Kette schon in der Hand hat. `SetAllocation` hat die Prüfung deshalb von Hand nachgebaut.

Fundstellen: `CreateNode` lädt die Kette für `managesNode` (op_lifecycle.go:125) und danach noch einmal (op_lifecycle.go:136) sowie ein drittes Mal in `normalizeCreateMembers` (op_lifecycle.go:301–315, inkl. eigenem `GetNode` des Elternknotens); `RequestChange` ruft `managesNode` (op_lifecycle.go:671), `parentChainNodes` (:686), `managesParentChain` (:740) und ggf. erneut (:824); Handnachbau in `SetAllocation` (op_allocations.go:91–97) und `listAllocationSources` (op_allocations.go:190).

Vorgeschlagene Abstraktion:

```go
func managesInChain(caller common.TokenSet, chain []model.Node) bool        // chain = Eltern aufwärts
func managesNodeIn(caller common.TokenSet, n *model.Node, chain []model.Node) bool
```

`managesNode`/`managesParentChain` bleiben als dünne Lader für Stellen ohne Kette.

Nutzen: mittel (weniger Store-Zugriffe pro Anfrage, eine Implementierung der Regel aus ARCHITECTURE.md:56). Risiko: mittel; Autorisierung. Die Kette muss unter derselben Sperre geladen sein wie heute. Aufwand: S–M.

Nötige Tests: `security_regression_test.go` und `rule_authz_project_admins_test.go` laufen unverändert; ergänzen: Zuweisung durch Verwalter eines Zwischenbudgets wird abgelehnt, durch Verwalter des Quellbudgets angenommen.

### 8. Feldlisten der Anfragen an einer Stelle führen

Problem: Wer ein Budget-Feld hinzufügt, muss es heute an mindestens vier Listen nachtragen, sonst greift die falsche Berechtigung oder die Kette wird nicht geladen.

Fundstellen: `isLeafDirectEdit` (op_lifecycle.go:357–368), `wantsPolicyEdit`/`wantsCapacityEdit` (op_lifecycle.go:407–413), die Bedingung zum Laden der Kette (op_lifecycle.go:491), die Ablehnung von Budget-Feldern auf Projekten in `CreateNode` (op_lifecycle.go:52–71). Zusätzlich sind die Löschsemantiken uneinheitlich: sechs Felder haben ein eigenes `Clear*`-Flag (`ClearAutoApprove`, `ClearMaxProjectTermDays`, `ClearWorkspaceScratchGB`, `ClearBatchPriority`, `ClearBatchJobs`, `ClearTerminationDate`; op_requests.go), `ReleaseMax` wird dagegen durch ein leeres Objekt gelöscht (`Normalized`, release.go:59–64).

Vorgeschlagene Abstraktion: Methoden auf der Anfrage, die die Klassifikation einmal festhalten:

```go
func (r UpdateNodeRequest) policyEdit() bool
func (r UpdateNodeRequest) capacityEdit() bool
func (r UpdateNodeRequest) needsChain() bool
func (r CreateNodeRequest) budgetOnlyField() (name string, set bool)
```

Ein Wechsel auf eine Dreiwertigkeit pro Feld (fehlt / `null` = löschen / Wert) wäre sauberer, ändert aber die öffentliche API und den generierten Client (siehe offene Fragen).

Nutzen: mittel (Wartbarkeit, keine Zeileneinsparung). Risiko: niedrig. Aufwand: S.

Nötige Tests: ein Reflexionstest, der jedes Feld von `UpdateNodeRequest` genau einer Klasse zuordnet (analog zum vorhandenen `mcp_input_drift_test.go`), damit ein neues Feld nicht unklassifiziert bleibt.

### 9. Boilerplate in HTTP-Handlern und MCP-Werkzeugen

Problem: Die schreibenden Knoten-Handler haben alle dieselbe Form (binden, Auth-Kontext, Service-Aufruf, `respondError`, JSON), die MCP-Werkzeuge ebenso (loggen, Service-Aufruf, `mcpServiceError`, `toMCPProject`). Außerdem kodiert ein MCP-Werkzeug eine Baumregel selbst.

Fundstellen: in api_nodes.go 23 Aufrufe von `mustGetAuthContext` und 11 identische `ShouldBindJSON`-Blöcke (z. B. createNode :365–385, updateNode :405–425, requestNodeChange :445–465, reparentNode :636–656, transferNodeOwner :676–696, promoteNode :860–880); in mcp.go 15 Werkzeuge mit demselben Rumpf (z. B. :515–532, :540–552, :560–571, :709–720, :727–734). `create_budget` setzt `AllowSubBudgetRequests=false` selbst, sobald ein Pro-Kopf-Limit gesetzt ist (mcp.go:692–698), statt sich auf `validateSubBudgetRequests` (rule_autoapprove.go:21–26) zu verlassen; der HTTP-Pfad tut das nicht, dort wird die Anfrage abgelehnt.

Vorgeschlagene Abstraktion:

```go
func nodeWrite[Req any](cfg APIConfig, status int, call func(c *gin.Context, req Req, auth AuthContext) (model.Node, error)) gin.HandlerFunc
func nodeTool[In any](s *mcp.Server, cfg APIConfig, caller mcpCaller, log *zap.SugaredLogger, t *mcp.Tool, call func(In) (model.Node, error), what func(In) string)
```

Swagger-Annotationen bleiben an den benannten Funktionen. Die Sonderregel in `create_budget` entweder in den Service verlegen (Default „keine Unterbudget-Anfragen“, wenn ein Pro-Kopf-Limit gesetzt wird und das Feld fehlt) oder bewusst nur in MCP belassen und kommentieren.

Nutzen: mittel (rund 150 Zeilen). Risiko: niedrig (rein mechanisch, durch `api_nodes_test.go` und `mcp_test.go` abgedeckt). Aufwand: M.

Nötige Tests: je Handler ein Test für 400 bei kaputtem JSON und 401 ohne Auth-Kontext (heute nur teilweise vorhanden); `create_budget` per MCP und per HTTP mit Pro-Kopf-Limit ohne `allow_sub_budget_requests` (bewusst festhalten, welches Verhalten gewollt ist).

### 10. Kleine generische Hilfen statt Kopien

Problem: Zeiger-Kopien und „kleinerer von zwei optionalen Werten“ sind mehrfach identisch implementiert.

Fundstellen: `cloneIntPtr` und `cloneStringPtr` (op_lifecycle.go:1103–1119), `cloneInt` (model/release.go:50–56), `cloneString` (model/batch_priority.go:51–57); „kleinstes `*int`“ in `StricterScratch` (scratch.go:13–22), `stricterIdle` (release.go:67–76) und `chainMaxTerm` (rule_terms.go:97–105).

Vorgeschlagene Abstraktion (Go 1.26, Generics verfügbar):

```go
func ClonePtr[T any](v *T) *T
func MinPtr[T cmp.Ordered](a, b *T) *T   // nil setzt nichts
```

Nutzen: niedrig bis mittel (rund 40 Zeilen). Risiko: sehr niedrig. Aufwand: S.

Nötige Tests: keine neuen; die bestehenden Tests zu Scratch, Release-Maxima und Laufzeit genügen.

## Weitere Funde

| Fund | Fundstelle | Bewertung |
|---|---|---|
| Doku-Kommentar von `bindingInChain` steht über `withDefaultAvailabilities`, die Funktion selbst hat keinen | rule_limits.go:283–286, :324 | trivial, S |
| Doku-Kommentare von `quotaFits` und `shortfall` sind ineinandergeschoben | calc_rollup.go:376–389 | trivial, S (entfällt mit Maßnahme 4) |
| `isBool` sucht linear im Katalog und wird in Schleifen über Ressourcen aufgerufen; `validateLeafAvailabilities` ruft `s.isBool(r.ID)`, obwohl `r` vorliegt | rule_limits.go:235–242, :369 | gering (Katalog klein), S |
| Drei Kettenläufer mit unterschiedlicher Zyklusbehandlung: Fehler (`nodeChain`), stilles Ende (`ancestry.chain`, `aiaas.chainOf`) | rule_authz.go:13–33; calc_decorate.go:173–190; aiaas/mapping.go:229–242 | bewusst? klären, S |
| Mehrere Statusmengen nebeneinander: `liveProjectStatuses`, `chargedStatuses`, `model.ActiveStatuses`, `RetiredStatuses` | op_lifecycle.go:352; calc_rollup.go:243–253; model.go | fachlich verschieden, nur dokumentieren |
| Status-Prüfung des Elternbudgets doppelt (in `leafChangeDecision` und in `autoApprovable`) | op_lifecycle.go:893; rule_autoapprove.go:33 | gering |
| Löschdatum im Reconciler mit `time.Now()` in lokaler Zone und Literal `"2006-01-02"`; gelesen wird es als UTC mit `time.DateOnly` | reconciler/released.go:171; reconciler/purge.go:167–168 | Randfall um Mitternacht, falls der Pod nicht in UTC läuft; S |
| `InArea` vergleicht mit dem Literal `"openstack"` statt `common.ProviderOpenStack` | common/projects.go:163 | trivial |
| `containsString` ist nur ein Wrapper um `slices.Contains` | nodestore/store.go:170–172 | trivial |
| Keine gemeinsamen Vertragstests für `NodeQuery` in Memory- und Postgres-Store; Tests des Postgres-Stores betreffen nur Migrationen, alle Baumtests laufen gegen den Memory-Store | nodestore/store.go:140–168; postgres.go:300–340 | mittleres Risiko bei Filteränderungen; M |
| `request_project_change` (MCP) kann Ende und Mitglieder nicht ändern; bewusst als „not offered yet“ markiert | mcp.go:292–296; mcp_input_drift_test.go:72 | Funktionslücke, kein Duplikat |
| Pro-Kopf-Verbrauch zählt nur direkte Kinder des Budgets, nicht Projekte in Unterbudgets | calc_rollup.go:218–236 | dokumentiert (ARCHITECTURE.md:57), nur festhalten |
| Validierungsfehler ohne Sentinel werden pauschal 400 | webserver/errors.go:14–30 | bewusste Konvention, beibehalten |

## Empfohlene Reihenfolge

1. Charakterisierungstests schreiben, ohne Produktionscode zu ändern: Tabellentests für `parseEnd`, `normalizeEnd`, `dateOnly`, `sameEnd`, `endsNoLater`, `chainEnd` (inkl. unlesbar und Offset), Sortierung `ChildSortEnd` mit gemischten Schreibweisen, `aiaas.EndOf` mit Offset. Einige werden das heutige Fehlverhalten festhalten; diese als bekannt markieren.
2. Offene Fragen 1–3 (Vergleich nach Tag oder Zeitpunkt, Offsets, kanonische Schreibweise) entscheiden.
3. Paket `enddate` einführen und die Baumfunktionen (`parseEnd`, `normalizeEnd`, `dateOnly`, `sameEnd`, `endsNoLater`, `chainEnd`) darauf umstellen. Ein Commit, Tests aus Schritt 1 werden grün bzw. bewusst angepasst.
4. Sortierung in `op_children.go` und `aiaas.parseDay`/`EndOf` auf `enddate` umstellen. Eigener Commit.
5. MCP-Beschreibung zu `termination_date` korrigieren (mcp.go:288).
6. Startmigration `normalizeEndDates` (Maßnahme 2) mit Migrationstest; danach ein Release, das nur das enthält.
7. Uhr im Service injizierbar machen und `endBound`/`fitNodeEnd` einführen (Maßnahme 3).
8. `mutateLiveSubtree` für die beiden Kürzungsfunktionen (Maßnahme 6).
9. Limit-Prädikate (`Holds`, `grantsAvailability`, `shortfall` statt `quotaFits`, `grows`) einführen (Maßnahme 4), je Prädikat ein Commit.
10. `ancestry` auf schlanke Knoten umstellen und `inherited*` über `model.Resolve*` (Maßnahme 5); `ClonePtr`/`MinPtr` (Maßnahme 10) im selben Zug.
11. Rechteprüfung auf geladener Kette (Maßnahme 7), mit den Sicherheitstests als Netz.
12. Klassifikation der Anfragefelder plus Reflexionstest (Maßnahme 8).
13. Handler- und MCP-Boilerplate (Maßnahme 9), zuletzt, weil rein mechanisch und am wenigsten riskant für die Fachlogik.

## Offene Fragen

1. Vergleichen Enddaten nach Kalendertag oder nach Zeitpunkt? Heute nach Zeitpunkt, aber ein reines Datum gilt als Mitternacht UTC (rule_terms.go:14), das Laufzeitende dagegen als 23:59:59 UTC (rule_terms.go:107–115). Ein Projekt mit `…T00:00:00.000Z` und eine Mitgliederänderung, die `…T23:59:59Z` desselben Tages mitschickt, gilt damit als Verlängerung. Ein Vergleich nach UTC-Tag würde genau diese Fehlerklasse ausschließen.
2. Sollen Offsets (`+02:00`) angenommen und nach UTC umgerechnet, angenommen und wörtlich gespeichert (heutiges Verhalten) oder abgelehnt werden? Und gilt ein Tag in UTC oder in Europe/Berlin?
3. Welche Schreibweise ist kanonisch im Speicher: `2006-01-02T15:04:05.000Z` (Formulare, `normalizeEnd`) oder `time.RFC3339` (Laufzeitende, Seed, Zeitstempel im Verlauf)? Oder nur `2006-01-02`, wenn Enddaten fachlich Tage sind?
4. Was soll mit einem unlesbaren gespeicherten Ende passieren: begrenzt nichts (heute `chainEnd`), gilt als „später“ (heute `endsNoLater`) oder als Fehler, der einen Menschen braucht? Nach der Migration in Maßnahme 2 sollte es keine mehr geben; die Semantik sollte trotzdem eine einzige sein.
5. Soll der Bestand migriert werden (Maßnahme 2), und sollen dabei auch `pending.termination_date` und die OpenStack-Tags angeglichen werden? Der Verlauf bleibt nach diesem Vorschlag unangetastet.
6. Soll das Löschen von Feldern in der API dreiwertig werden (`null` = löschen) statt über `clear_*`-Flags, und soll `release_max` dabei dieselbe Regel bekommen? Das betrifft den generierten Client (`client-npm`) und die UI.
7. Ist das stille Setzen von `allow_sub_budget_requests=false` in `create_budget` (MCP) gewollt, und soll der HTTP-Pfad es ebenso tun oder weiter ablehnen?
8. Sollen Verfügbarkeiten an Budgets mit `-1` überhaupt als „gewährt“ gelesen werden (drei Stellen tun das, zwei nicht), obwohl `validateAvailabilities` `-1` ablehnt? Wenn `-1` dort nie vorkommen kann, lässt sich die Sonderbehandlung streichen.

## Entscheidungen (10.11.2026)

1. Enddaten werden nach Kalendertag verglichen, in Europe/Berlin.
2. Gespeichert wird nur das Datum (`2006-01-02`). Angaben mit Uhrzeit oder Zeitzonenversatz werden angenommen und als das darin geschriebene Datum gelesen, ohne Umrechnung nach Berlin (Abweichung vom ersten Wortlaut: das Laufzeitende `…T23:59:59Z` wäre in Berlin schon der Folgetag). „Heute“ und das Ende eines Tages zählen in Europe/Berlin. Umsetzung: `cloud-self-service-golib/day`.
3. Ein unlesbares gespeichertes Ende gilt überall einheitlich als „später“ (lieber eine Bestätigung zu viel).
4. Der Bestand wird einmalig migriert: Enddaten der Knoten, `pending.termination_date` und die OpenStack-Tags; der Verlauf bleibt unverändert.
5. Felder werden in der API mit `null` gelöscht, die `clear_*`-Flags entfallen; `release_max` folgt derselben Regel. Client (`client-npm`) und UI werden zusammen umgestellt.
6. Fehlt `allow_sub_budget_requests=false` bei Grenzen je Person, lehnen HTTP und MCP das Budget einheitlich ab (kein stilles Setzen mehr).
7. Die Sonderbehandlung von `-1` bei Verfügbarkeiten an Budgets entfällt, nachdem geprüft ist, dass kein gespeichertes Budget `-1` enthält.
8. Gemeinsame Helfer (Wiederholen mit Backoff beim Start, Token-Prüfung, Lesen von Zeiträumen und Enddaten) kommen nach `cloud-self-service-golib`; os-mgt wartet damit beim Start ebenfalls auf Postgres.
9. Der Budgetbaum vergibt weiterhin nur Mengen je Zeitfenster (`24h`, `7d`, `30d`), keine Kontingente für die ganze Laufzeit.
