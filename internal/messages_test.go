package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The texts for people are pairs, German and English (golib msg). These constructors take the German text and then the English one, as their first two arguments or after a leading one (refuse's kind, respondBadRequest's context does not count: it takes a msg.Text).
var pairConstructors = map[string]int{
	"msg.T":      0,
	"msg.F":      0,
	"msg.Errorf": 0,
	"invalid":    0,
	"mcpRefusal": 0,
	"refuse":     1,
	"listResult": 3,
	"nodeResult": 3,
	// mcpServiceError(log, err, de, en, ...)
	"mcpServiceError": 2,
}

// storedGerman are German texts that are no answer of the API but stored, written once in the language of the platform (decided: stored history texts stay as they are): the notes the server writes into a node's history, and the purpose of imported projects and of the bootstrap's collection budget.
var storedGerman = []string{
	"Direkt angelegt von einer Person, die das Budget verwaltet",
	"Übernimmt das Limit des Budgets darüber",
	"Direkt übernommen (gibt Ressourcen zurück, endet früher oder ändert nur Mitglieder)",
	"Automatisch genehmigt (im Budget ist Platz)",
	"Automatisch genehmigt (innerhalb der Grenze pro Person)",
	"Endet mit dem Budget %q",
	"Projekte im Budget %q laufen höchstens %d Tage am Stück",
	"Sammelstelle für OpenStack-Projekte, die der Abgleich gefunden hat. Limit 0: Hier lässt sich nichts genehmigen — Importe in ein richtiges Budget übernehmen.",
	"OpenStack-Projekt: %s (%s)",
}

var (
	verb = regexp.MustCompile(`%(\[\d+\])?[-+# 0]*\d*(\.\d+)?[a-zA-Z]`)
	// looksGerman is a German text: an umlaut or ß, or one of the words every German sentence here has one of.
	looksGerman = regexp.MustCompile(`[äöüÄÖÜß]|\b(nicht|ist|darf|muss|kein|keine|sind|lässt|wird|werden|oder|und|der|die|das|dem|den|ein|eine|einem|einen|für|mit|nur|bereits|Anmeldung)\b`)
)

func calleeName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			return x.Name + "." + f.Sel.Name
		}
	case *ast.IndexExpr:
		if id, ok := f.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// stringLit is the value of a string literal; false for anything else.
func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

func verbs(s string) []string {
	v := verb.FindAllString(strings.ReplaceAll(s, "%%", ""), -1)
	slices.Sort(v)
	return v
}

// Every text for people the service can answer with has its English twin: every pair names both languages, with the same format verbs, and no German literal reaches people outside a pair. A German text that is stored instead is listed in storedGerman. Log messages and the tests are not looked at; what a test pins is the German text.
func TestMessagesForPeopleHaveAnEnglishTwin(t *testing.T) {
	fset := token.NewFileSet()
	pairs := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "generated_docs" || d.Name() == "dev") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		inPair := map[*ast.BasicLit]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// A log call is no answer to people.
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (strings.HasPrefix(sel.Sel.Name, "Info") || strings.HasPrefix(sel.Sel.Name, "Warn") || strings.HasPrefix(sel.Sel.Name, "Error") || strings.HasPrefix(sel.Sel.Name, "Debug")) {
				for _, a := range call.Args {
					if lit, ok := a.(*ast.BasicLit); ok {
						inPair[lit] = true
					}
				}
			}
			i, ok := pairConstructors[calleeName(call)]
			if !ok || len(call.Args) < i+2 {
				return true
			}
			de, okDE := stringLit(call.Args[i])
			en, okEN := stringLit(call.Args[i+1])
			if !okDE || !okEN {
				return true // a pair passed on (de, en parameters), checked where it is written
			}
			pairs++
			inPair[call.Args[i].(*ast.BasicLit)], inPair[call.Args[i+1].(*ast.BasicLit)] = true, true
			pos := fset.Position(call.Pos())
			switch {
			case de == "" || en == "":
				t.Errorf("%s: %s(%q, %q): a text for people needs German and English", pos, calleeName(call), de, en)
			case de == en:
				t.Errorf("%s: %s(%q, …): the English text is the German one", pos, calleeName(call), de)
			case !slices.Equal(verbs(de), verbs(en)):
				t.Errorf("%s: %s: the format verbs differ: %v in %q, %v in %q", pos, calleeName(call), verbs(de), de, verbs(en), en)
			case looksGerman.MatchString(en) && !strings.Contains(de, en):
				t.Errorf("%s: %s: the English text looks German: %q", pos, calleeName(call), en)
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || inPair[lit] {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || !looksGerman.MatchString(s) || slices.Contains(storedGerman, s) || strings.Contains(lit.Value, "`") && strings.Contains(s, "\n") {
				return true
			}
			t.Errorf("%s: %q looks like a German text for people outside a pair (msg.T, msg.F, msg.Errorf, refuse, invalid); give it its English twin, or list it in storedGerman if it is stored rather than answered", fset.Position(lit.Pos()), s)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d texts for people in German and English", pairs)
	if pairs < 150 {
		t.Fatalf("found only %d pairs; is the scan still looking at the right calls?", pairs)
	}
}
