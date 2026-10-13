package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Interface translations for the bar plugin.
//
// English is the source language and the key: the plugin calls tr("Volume of
// prompts") and falls back to that very text when a language has no entry. A
// language is a flat JSON object {"_name": "Native name", "<English>": "<text>"}.
// Keys starting with "_" are metadata ("_name", "_decimal"), never UI text.
//
// Two places hold languages:
//   - embedded in this binary (backend/i18n/*.json), shipped with the plugin;
//   - the user's directory (~/.config/qcy-omarchy/i18n/<lang>.json), outside the
//     plugin directory because the installer wipes that one on every install.
// User keys override the embedded ones, so a user file may be a partial patch.
//
// Everything here is offline: no daemon, no Bluetooth.

//go:embed i18n/*.json
var i18nFS embed.FS

const (
	i18nChoiceAuto  = "auto"
	i18nDefaultLang = "en"
	i18nMaxCodeLen  = 16
)

// i18nWarn receives warnings about unusable user files (stderr by default).
var i18nWarn = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "qcyctl: i18n: "+format+"\n", args...)
}

// canonLang validates a language code and returns it in canonical form
// (language lower case, 2-letter region upper case, 4-letter script title
// case, '_' turned into '-'). ok is false for anything that is not letters
// separated by '-' or '_', or that is longer than 16 bytes: the code ends up in
// a file name, so '/', '.', and friends must never pass.
func canonLang(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > i18nMaxCodeLen {
		return "", false
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' })
	// FieldsFunc swallows empty parts: reject "a--b", "-a", "a-" explicitly.
	if len(parts) == 0 || strings.Join(parts, "-") != strings.ReplaceAll(s, "_", "-") {
		return "", false
	}
	for i, p := range parts {
		for _, r := range p {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
				return "", false
			}
		}
		switch {
		case i == 0:
			parts[i] = strings.ToLower(p)
		case len(p) == 2:
			parts[i] = strings.ToUpper(p)
		case len(p) == 4:
			parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
		default:
			parts[i] = strings.ToLower(p)
		}
	}
	return strings.Join(parts, "-"), true
}

// normalizeLocale turns a POSIX locale ("pt_BR.UTF-8", "en_US@euro") into a
// language code ("pt-BR"). C, POSIX, empty or unparsable values mean English.
func normalizeLocale(loc string) string {
	loc = strings.TrimSpace(loc)
	if i := strings.IndexAny(loc, ".@"); i >= 0 {
		loc = loc[:i]
	}
	if loc == "" || strings.EqualFold(loc, "C") || strings.EqualFold(loc, "POSIX") {
		return i18nDefaultLang
	}
	if c, ok := canonLang(loc); ok {
		return c
	}
	return i18nDefaultLang
}

// systemLang reads the locale the way gettext does: LC_ALL, LC_MESSAGES, LANG.
func systemLang() string {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return normalizeLocale(v)
		}
	}
	return i18nDefaultLang
}

func qcyConfigDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" || !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "qcy-omarchy"), nil
}

func i18nUserDir() (string, error) {
	d, err := qcyConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "i18n"), nil
}

func i18nChoicePath() (string, error) {
	d, err := qcyConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "lang"), nil
}

// i18nMaxFileBytes caps a user translation file: far above any real one, far
// below something that would hurt the process.
const i18nMaxFileBytes = 1 << 20

// readCappedFile reads a regular file of at most max bytes. A FIFO, a device or
// a huge file would otherwise block or exhaust the process.
func readCappedFile(p string, max int64) ([]byte, error) {
	// Stat before Open: opening a FIFO would block until someone writes to it.
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if st.Size() > max {
		return nil, fmt.Errorf("file larger than %d bytes", max)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max+1))
}

// readLangChoice returns "auto" or a canonical code; a missing, empty or
// invalid file means auto.
func readLangChoice() string {
	p, err := i18nChoicePath()
	if err != nil {
		return i18nChoiceAuto
	}
	b, err := readCappedFile(p, 4<<10)
	if err != nil {
		return i18nChoiceAuto
	}
	s := strings.TrimSpace(string(b))
	if strings.EqualFold(s, i18nChoiceAuto) {
		return i18nChoiceAuto
	}
	if c, ok := canonLang(s); ok {
		return c
	}
	return i18nChoiceAuto
}

func writeLangChoice(choice string) error {
	p, err := i18nChoicePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(choice+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// parseLangFile decodes a flat string map. Entries with an empty value are
// dropped, so an untouched template line never blanks the UI.
func parseLangFile(b []byte) (map[string]string, error) {
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if v != "" {
			out[k] = v
		}
	}
	return out, nil
}

// embeddedLang returns the shipped layer for code, or nil.
func embeddedLang(code string) map[string]string {
	b, err := i18nFS.ReadFile("i18n/" + code + ".json")
	if err != nil {
		return nil
	}
	m, err := parseLangFile(b)
	if err != nil {
		// A broken shipped file is a build defect (TestEmbeddedLanguagesParse).
		i18nWarn("embedded language %s is invalid: %v", code, err)
		return nil
	}
	return m
}

// userLang returns the user's layer for code, or nil. An unreadable or
// invalid file is skipped with a warning.
func userLang(code string) map[string]string {
	dir, err := i18nUserDir()
	if err != nil {
		return nil
	}
	p := filepath.Join(dir, code+".json")
	b, err := readCappedFile(p, i18nMaxFileBytes)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			i18nWarn("ignoring %s: %v", p, err)
		}
		return nil
	}
	m, err := parseLangFile(b)
	if err != nil {
		i18nWarn("ignoring %s: %v", p, err)
		return nil
	}
	return m
}

// bundleFor merges the layers for a language, least specific first: the
// region-less language (pt for pt-BR), then the language itself; in each step
// the embedded text, then the user's. Missing layers are simply absent, so an
// unknown language yields an empty bundle (the UI then shows English).
func bundleFor(lang string) map[string]string {
	out := bundleExact(lang)
	// pt-PT with no pt-PT or pt file: a sibling of the same language (pt-BR)
	// reads better to the user than English.
	if len(out) == 0 {
		if i := strings.Index(lang, "-"); i > 0 {
			base := lang[:i]
			var sibs []string
			for _, l := range listLangs() {
				if l.Code != lang && strings.HasPrefix(l.Code, base+"-") {
					sibs = append(sibs, l.Code)
				}
			}
			sort.Strings(sibs)
			for _, c := range sibs {
				if m := bundleExact(c); len(m) > 0 {
					return m
				}
			}
		}
	}
	return out
}

func bundleExact(lang string) map[string]string {
	out := map[string]string{}
	codes := []string{lang}
	if i := strings.Index(lang, "-"); i > 0 {
		codes = []string{lang[:i], lang}
	}
	for _, c := range codes {
		for _, layer := range []map[string]string{embeddedLang(c), userLang(c)} {
			for k, v := range layer {
				out[k] = v
			}
		}
	}
	return out
}

type i18nLang struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	Builtin bool   `json:"builtin"`
	User    bool   `json:"user"`
}

// listLangs: English, the embedded languages and the user's, by code.
func listLangs() []i18nLang {
	byCode := map[string]*i18nLang{}
	get := func(code string) *i18nLang {
		l := byCode[code]
		if l == nil {
			l = &i18nLang{Code: code}
			byCode[code] = l
		}
		return l
	}
	en := get(i18nDefaultLang)
	en.Builtin = true
	en.Name = "English"

	if ents, err := i18nFS.ReadDir("i18n"); err == nil {
		for _, e := range ents {
			code, ok := langFromFile(e.Name())
			if !ok {
				continue
			}
			l := get(code)
			l.Builtin = true
			if n := embeddedLang(code)["_name"]; n != "" {
				l.Name = n
			}
		}
	}
	if dir, err := i18nUserDir(); err == nil {
		if ents, err := os.ReadDir(dir); err == nil {
			for _, e := range ents {
				if e.IsDir() {
					continue
				}
				code, ok := langFromFile(e.Name())
				if !ok {
					continue
				}
				m := userLang(code)
				if m == nil {
					continue // invalid file: not offered
				}
				l := get(code)
				l.User = true
				if n := m["_name"]; n != "" {
					l.Name = n
				}
			}
		}
	}
	out := make([]i18nLang, 0, len(byCode))
	for _, l := range byCode {
		if l.Name == "" {
			l.Name = l.Code
		}
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Code == i18nDefaultLang) != (out[j].Code == i18nDefaultLang) {
			return out[i].Code == i18nDefaultLang
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// langFromFile maps "pt-BR.json" to "pt-BR"; only canonical names count, so
// "pt_br.json" or "../x.json" are never offered.
func langFromFile(name string) (string, bool) {
	if !strings.HasSuffix(name, ".json") {
		return "", false
	}
	base := strings.TrimSuffix(name, ".json")
	c, ok := canonLang(base)
	if !ok || c != base {
		return "", false
	}
	return c, true
}

// resolveActive maps a wanted code onto what is available: exact, then without
// region, then English.
func resolveActive(want string, langs []i18nLang) string {
	has := map[string]bool{}
	for _, l := range langs {
		has[l.Code] = true
	}
	if has[want] {
		return want
	}
	if i := strings.Index(want, "-"); i > 0 && has[want[:i]] {
		return want[:i]
	}
	return i18nDefaultLang
}

// effectiveLang is the code the bundle is built for: the saved choice, or the
// system's locale when the choice is auto.
func effectiveLang() string {
	c := readLangChoice()
	if c == i18nChoiceAuto {
		return systemLang()
	}
	return c
}

// templateKeys lists every English key the shipped languages translate.
func templateKeys() []string {
	set := map[string]bool{}
	if ents, err := i18nFS.ReadDir("i18n"); err == nil {
		for _, e := range ents {
			code, ok := langFromFile(e.Name())
			if !ok {
				continue
			}
			for k := range embeddedLang(code) {
				if !strings.HasPrefix(k, "_") {
					set[k] = true
				}
			}
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func jsonStr(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimRight(buf.String(), "\n")
}

// renderTemplate: a translation file ready to edit. Each value starts as the
// English text; "_name" is the language's own name, "_decimal" its decimal
// separator.
func renderTemplate(keys []string) string {
	var b strings.Builder
	b.WriteString("{\n")
	b.WriteString("  \"_name\": \"\",\n")
	b.WriteString("  \"_decimal\": \".\"")
	for _, k := range keys {
		b.WriteString(",\n  " + jsonStr(k) + ": " + jsonStr(k))
	}
	b.WriteString("\n}\n")
	return b.String()
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// splitJSONFlag pulls --json out of args and rejects other flags.
func splitJSONFlag(args []string) (rest []string, asJSON bool, err error) {
	for _, a := range args {
		switch {
		case a == "--json":
			asJSON = true
		case strings.HasPrefix(a, "-") && a != "-":
			return nil, false, fmt.Errorf("unknown flag %q", a)
		default:
			rest = append(rest, a)
		}
	}
	return rest, asJSON, nil
}

func cmdI18n(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: qcyctl i18n <list|bundle|template|set> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		rest, asJSON, err := splitJSONFlag(rest)
		if err != nil {
			return err
		}
		if len(rest) != 0 {
			return errors.New("usage: qcyctl i18n list [--json]")
		}
		langs := listLangs()
		choice := readLangChoice()
		sys := systemLang()
		want := sys
		if choice != i18nChoiceAuto {
			want = choice
		}
		active := resolveActive(want, langs)
		if asJSON {
			return writeJSON(map[string]any{
				"choice":    choice,
				"active":    active,
				"system":    sys,
				"languages": langs,
			})
		}
		fmt.Printf("choice: %s\n", choice)
		for _, l := range langs {
			mark := " "
			if l.Code == active {
				mark = "*"
			}
			src := "builtin"
			switch {
			case l.Builtin && l.User:
				src = "builtin+user"
			case l.User:
				src = "user"
			}
			fmt.Printf("%s %-10s %s (%s)\n", mark, l.Code, l.Name, src)
		}
		return nil

	case "bundle":
		rest, _, err := splitJSONFlag(rest) // output is always JSON
		if err != nil {
			return err
		}
		if len(rest) > 1 {
			return errors.New("usage: qcyctl i18n bundle [<lang>] [--json]")
		}
		lang := ""
		if len(rest) == 1 {
			c, ok := canonLang(rest[0])
			if !ok {
				return fmt.Errorf("invalid language code %q", rest[0])
			}
			lang = c
		} else {
			lang = effectiveLang()
		}
		return writeJSON(bundleFor(lang))

	case "template":
		if len(rest) != 0 {
			return errors.New("usage: qcyctl i18n template")
		}
		_, err := fmt.Print(renderTemplate(templateKeys()))
		return err

	case "set":
		if len(rest) != 1 {
			return errors.New("usage: qcyctl i18n set <lang|auto>")
		}
		choice := rest[0]
		if strings.EqualFold(strings.TrimSpace(choice), i18nChoiceAuto) {
			choice = i18nChoiceAuto
		} else {
			c, ok := canonLang(choice)
			if !ok {
				return fmt.Errorf("invalid language code %q (letters and '-' only, up to %d characters)", choice, i18nMaxCodeLen)
			}
			choice = c
		}
		if err := writeLangChoice(choice); err != nil {
			return err
		}
		fmt.Println(choice)
		return nil
	}
	return fmt.Errorf("unknown i18n command %q (list, bundle, template, set)", sub)
}
