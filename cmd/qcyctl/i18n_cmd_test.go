package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// isolate points the config dir at a fresh temp tree and clears the locale.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		t.Setenv(k, "")
	}
	return dir
}

func writeUserLang(t *testing.T, cfg, name, content string) {
	t.Helper()
	d := filepath.Join(cfg, "qcy-omarchy", "i18n")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func captureWarnings(t *testing.T) *[]string {
	t.Helper()
	var got []string
	old := i18nWarn
	i18nWarn = func(format string, args ...any) { got = append(got, format) }
	t.Cleanup(func() { i18nWarn = old })
	return &got
}

func TestNormalizeLocale(t *testing.T) {
	cases := map[string]string{
		"pt_BR.UTF-8":       "pt-BR",
		"pt_BR":             "pt-BR",
		"en_US.UTF-8":       "en-US",
		"de_DE@euro":        "de-DE",
		"fr":                "fr",
		"pt-br":             "pt-BR",
		"zh_Hant_TW":        "zh-Hant-TW",
		"C":                 "en",
		"C.UTF-8":           "en",
		"POSIX":             "en",
		"":                  "en",
		"../etc/x":          "en",
		"a_b_c_d_e_f_g_h_i": "en", // too long
	}
	for in, want := range cases {
		if got := normalizeLocale(in); got != want {
			t.Errorf("normalizeLocale(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSystemLangPrecedence(t *testing.T) {
	isolate(t)
	if got := systemLang(); got != "en" {
		t.Fatalf("empty env: %q", got)
	}
	t.Setenv("LANG", "pt_BR.UTF-8")
	if got := systemLang(); got != "pt-BR" {
		t.Fatalf("LANG: %q", got)
	}
	t.Setenv("LC_MESSAGES", "es_ES.UTF-8")
	if got := systemLang(); got != "es-ES" {
		t.Fatalf("LC_MESSAGES: %q", got)
	}
	t.Setenv("LC_ALL", "C")
	if got := systemLang(); got != "en" {
		t.Fatalf("LC_ALL=C: %q", got)
	}
}

func TestCanonLangRejects(t *testing.T) {
	for _, bad := range []string{"", "../x", "a/b", "pt BR", "pt--BR", "-pt", "pt-", ".", "..", "pt.json", "abcdefghijklmnopq", "pt\x00", "p1"} {
		if c, ok := canonLang(bad); ok {
			t.Errorf("canonLang(%q) accepted as %q", bad, c)
		}
	}
	for in, want := range map[string]string{"pt_br": "pt-BR", "EN": "en", "zh-hant": "zh-Hant"} {
		if c, ok := canonLang(in); !ok || c != want {
			t.Errorf("canonLang(%q) = %q,%v want %q", in, c, ok, want)
		}
	}
}

func TestBundleEmbeddedAndFallbackToBaseLanguage(t *testing.T) {
	cfg := isolate(t)
	pt := bundleFor("pt-BR")
	if pt["Refresh"] != "Atualizar" || pt["_decimal"] != "," {
		t.Fatalf("embedded pt-BR missing: %v", pt["Refresh"])
	}
	if len(bundleFor("en")) != 0 {
		t.Fatal("English must be an empty bundle")
	}
	if len(bundleFor("xx")) != 0 {
		t.Fatal("unknown language must be empty")
	}
	// pt-PT has no file of its own: falls back to the user's pt, without
	// dropping the shipped pt-BR (it is a different language, so not used).
	writeUserLang(t, cfg, "pt.json", `{"Refresh":"Recarregar","Save":"Guardar"}`)
	b := bundleFor("pt-PT")
	if b["Refresh"] != "Recarregar" || b["Save"] != "Guardar" {
		t.Fatalf("pt-PT did not fall back to pt: %v", b)
	}
	if _, ok := b["Cancel"]; ok {
		t.Fatal("pt-PT must not inherit pt-BR")
	}
	// pt-BR layers pt (user) under the shipped pt-BR.
	b = bundleFor("pt-BR")
	if b["Save"] != "Salvar" || b["Refresh"] != "Atualizar" {
		t.Fatalf("pt-BR specific must win over pt: %v", b)
	}
}

func TestBundleUserOverridesEmbedded(t *testing.T) {
	cfg := isolate(t)
	writeUserLang(t, cfg, "pt-BR.json", `{"Refresh":"Recarregar","Brand new":"Novo","Save":""}`)
	b := bundleFor("pt-BR")
	if b["Refresh"] != "Recarregar" {
		t.Fatalf("user did not override: %q", b["Refresh"])
	}
	if b["Brand new"] != "Novo" {
		t.Fatal("user-only key missing")
	}
	if b["Save"] != "Salvar" {
		t.Fatalf("empty user value must not blank the shipped text: %q", b["Save"])
	}
	if b["Cancel"] != "Cancelar" {
		t.Fatal("untouched shipped key lost")
	}
}

func TestInvalidUserFileIgnoredWithWarning(t *testing.T) {
	cfg := isolate(t)
	warns := captureWarnings(t)
	writeUserLang(t, cfg, "es.json", `{not json`)
	writeUserLang(t, cfg, "fr.json", `{"Save": 3}`)
	if b := bundleFor("es"); len(b) != 0 {
		t.Fatalf("invalid file produced entries: %v", b)
	}
	if len(*warns) == 0 {
		t.Fatal("no warning for an invalid file")
	}
	if b := bundleFor("fr"); len(b) != 0 {
		t.Fatalf("non-string values accepted: %v", b)
	}
	for _, l := range listLangs() {
		if l.Code == "es" || l.Code == "fr" {
			t.Fatalf("invalid file offered as a language: %+v", l)
		}
	}
	// A valid language keeps working next to broken ones.
	if bundleFor("pt-BR")["Refresh"] != "Atualizar" {
		t.Fatal("shipped language broken by a bad user file")
	}
}

func TestListLangs(t *testing.T) {
	cfg := isolate(t)
	writeUserLang(t, cfg, "es.json", `{"_name":"Español","Save":"Guardar"}`)
	writeUserLang(t, cfg, "pt-BR.json", `{"Save":"Gravar"}`)
	writeUserLang(t, cfg, "notes.txt", `x`)
	writeUserLang(t, cfg, "pt_br.json", `{}`) // not canonical: ignored
	got := map[string]i18nLang{}
	var order []string
	for _, l := range listLangs() {
		got[l.Code] = l
		order = append(order, l.Code)
	}
	if order[0] != "en" || got["en"].Name != "English" {
		t.Fatalf("English must lead: %v", order)
	}
	if l := got["pt-BR"]; l.Name != "Português (Brasil)" || !l.Builtin || !l.User {
		t.Fatalf("pt-BR: %+v", l)
	}
	if l := got["es"]; l.Name != "Español" || l.Builtin || !l.User {
		t.Fatalf("es: %+v", l)
	}
	if _, ok := got["pt_br"]; ok {
		t.Fatal("non-canonical file name offered")
	}
}

func TestSetAndReadChoice(t *testing.T) {
	cfg := isolate(t)
	if got := readLangChoice(); got != "auto" {
		t.Fatalf("default: %q", got)
	}
	if err := cmdI18n([]string{"set", "pt_br"}); err != nil {
		t.Fatal(err)
	}
	if got := readLangChoice(); got != "pt-BR" {
		t.Fatalf("after set: %q", got)
	}
	b, err := os.ReadFile(filepath.Join(cfg, "qcy-omarchy", "lang"))
	if err != nil || strings.TrimSpace(string(b)) != "pt-BR" {
		t.Fatalf("file: %q %v", b, err)
	}
	if err := cmdI18n([]string{"set", "AUTO"}); err != nil {
		t.Fatal(err)
	}
	if got := readLangChoice(); got != "auto" {
		t.Fatalf("after auto: %q", got)
	}
	// A choice file edited by hand into nonsense falls back to auto.
	if err := os.WriteFile(filepath.Join(cfg, "qcy-omarchy", "lang"), []byte("../../x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readLangChoice(); got != "auto" {
		t.Fatalf("garbage choice: %q", got)
	}
}

func TestSetRejectsMaliciousCode(t *testing.T) {
	cfg := isolate(t)
	for _, bad := range []string{"../x", "a/b", "", "..", "pt-BR/../../x", "x.json"} {
		if err := cmdI18n([]string{"set", bad}); err == nil {
			t.Errorf("set %q accepted", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg, "qcy-omarchy", "lang")); err == nil {
		t.Fatal("a rejected set wrote the choice file")
	}
	// bundle must refuse it too, rather than build a path from it.
	if err := cmdI18n([]string{"bundle", "../x"}); err == nil {
		t.Fatal("bundle ../x accepted")
	}
}

func TestEffectiveLangFollowsChoiceThenSystem(t *testing.T) {
	isolate(t)
	t.Setenv("LANG", "pt_BR.UTF-8")
	if got := effectiveLang(); got != "pt-BR" {
		t.Fatalf("auto: %q", got)
	}
	if err := writeLangChoice("es"); err != nil {
		t.Fatal(err)
	}
	if got := effectiveLang(); got != "es" {
		t.Fatalf("saved: %q", got)
	}
	langs := listLangs()
	if got := resolveActive("pt-PT", langs); got != "en" {
		t.Fatalf("unavailable resolves to en, got %q", got)
	}
	if got := resolveActive("pt-BR", langs); got != "pt-BR" {
		t.Fatalf("pt-BR: %q", got)
	}
}

func TestTemplateIsValidJSONWithAllKeys(t *testing.T) {
	isolate(t)
	out := renderTemplate(templateKeys())
	var m map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("template is not valid JSON: %v", err)
	}
	if _, ok := m["_name"]; !ok {
		t.Fatal("no _name")
	}
	if m["Refresh"] != "Refresh" {
		t.Fatalf("values must start as the English text: %q", m["Refresh"])
	}
	if len(m) < 100 {
		t.Fatalf("template too small: %d", len(m))
	}
}

func TestEmbeddedLanguagesParse(t *testing.T) {
	ents, err := i18nFS.ReadDir("i18n")
	if err != nil || len(ents) == 0 {
		t.Fatalf("no embedded languages: %v", err)
	}
	for _, e := range ents {
		b, err := i18nFS.ReadFile("i18n/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		m, err := parseLangFile(b)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if m["_name"] == "" {
			t.Errorf("%s: missing _name", e.Name())
		}
		if code, ok := langFromFile(e.Name()); !ok {
			t.Errorf("%s: not a canonical language file name (%q)", e.Name(), code)
		}
	}
}

// Every English key the QML asks for must have a pt-BR entry, and pt-BR must
// not carry keys the UI no longer uses.
func TestQmlKeysMatchPtBR(t *testing.T) {
	re := regexp.MustCompile(`\btr\(\s*("(?:[^"\\]|\\.)*")`)
	used := map[string]bool{}
	for _, f := range []string{"BarWidget.qml", "Qcy.qml"} {
		b, err := os.ReadFile(filepath.Join("..", "..", f))
		if err != nil {
			t.Skipf("plugin sources not available: %v", err)
		}
		for _, m := range re.FindAllSubmatch(b, -1) {
			k, err := strconv.Unquote(string(m[1]))
			if err != nil {
				t.Fatalf("%s: cannot decode key %s: %v", f, m[1], err)
			}
			used[k] = true
		}
	}
	if len(used) == 0 {
		t.Fatal("no tr() keys found in the QML")
	}
	pt := embeddedLang("pt-BR")
	var missing, stale []string
	for k := range used {
		if _, ok := pt[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range pt {
		if !strings.HasPrefix(k, "_") && !used[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("keys used in QML but missing from pt-BR.json: %q", missing)
	}
	if len(stale) > 0 {
		t.Errorf("keys in pt-BR.json not used in QML: %q", stale)
	}
	// Placeholders must survive translation.
	ph := regexp.MustCompile(`\{[a-zA-Z]+\}`)
	for k, v := range pt {
		a, b := ph.FindAllString(k, -1), ph.FindAllString(v, -1)
		sort.Strings(a)
		sort.Strings(b)
		if strings.Join(a, ",") != strings.Join(b, ",") {
			t.Errorf("placeholders differ for %q: %v vs %v", k, a, b)
		}
	}
}

func TestListJSONShape(t *testing.T) {
	isolate(t)
	t.Setenv("LANG", "pt_BR.UTF-8")
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := cmdI18n([]string{"list", "--json"})
	w.Close()
	os.Stdout = old
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.ReadFrom(r)
	var v struct {
		Choice, Active, System string
		Languages              []i18nLang
	}
	if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
		t.Fatalf("%v: %s", err, buf.String())
	}
	if v.Choice != "auto" || v.Active != "pt-BR" || v.System != "pt-BR" || len(v.Languages) < 2 {
		t.Fatalf("%+v", v)
	}
}

func TestRegionlessSiblingFallbackAndCappedFiles(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	// pt-PT has no file of its own: pt-BR is the sibling of the same language.
	if m := bundleFor("pt-PT"); len(m) == 0 || m["Balance"] == "" {
		t.Fatalf("pt-PT bundle: %d keys", len(m))
	}
	if m := bundleFor("xx-YY"); len(m) != 0 {
		t.Fatalf("unknown language got %d keys", len(m))
	}
	dir := filepath.Join(cfg, "qcy-omarchy", "i18n")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// a FIFO named like a language file must not block the read
	if err := syscall.Mkfifo(filepath.Join(dir, "de.json"), 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan struct{})
	go func() { _ = userLang("de"); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("userLang blocked on a FIFO")
	}
	// an oversized file is ignored
	big := make([]byte, i18nMaxFileBytes+10)
	if err := os.WriteFile(filepath.Join(dir, "fr.json"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if m := userLang("fr"); m != nil {
		t.Fatalf("oversized file loaded: %d keys", len(m))
	}
}
