package configmerge_test

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/paisans-software/paisans-stack/internal/configmerge"
)

// The real rendered files the format specific tests are asserted against. A
// synthetic fixture is written to suit the writer; a rendered one is what the
// writer will actually meet.
const (
	goldenEnv  = "../render/testdata/golden/home-a/srv/talk/.env"
	goldenINI  = "../render/testdata/golden/home-a/srv/blog/config.ini"
	goldenYAML = "../render/testdata/golden/vm/srv/chat/homeserver.yaml"
	goldenJSON = "../render/testdata/golden/home-b/srv/web/config.json"
)

// The line each format writes above the keys it adds, so a reader of the
// rendered file can tell them from what the template wrote.
const (
	label     = "Set by `config` in paisans.yaml, not by a template."
	iniLabel  = "; " + label
	yamlLabel = "# " + label
)

func readGolden(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type mergeFunc func(string, map[string]any) (string, error)

var formats = []struct {
	name   string
	merge  mergeFunc
	golden string
	key    string // a key that is valid for the format and absent from its golden
}{
	{"env", configmerge.Env, goldenEnv, "KBIN_META_TITLE"},
	{"ini", configmerge.INI, goldenINI, "app.max_blogs"},
	{"yaml", configmerge.YAML, goldenYAML, "url_preview_enabled"},
	{"json", configmerge.JSON, goldenJSON, "show_labs_settings_extra"},
}

// Env is line oriented and has no nesting. The block is labelled so a reader
// of the rendered file knows where the values came from.
func TestEnvAppendsALabelledBlock(t *testing.T) {
	rendered := "# Rendered by paisans. Do not edit.\nDATABASE_URL=postgres://x\n"
	out, err := configmerge.Env(rendered, map[string]any{
		"KBIN_META_TITLE": "A place",
		"KBIN_META_DESC":  "Talk here",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "DATABASE_URL=postgres://x") {
		t.Error("the rendered content must survive")
	}
	if !strings.Contains(out, "KBIN_META_DESC=Talk here") || !strings.Contains(out, "KBIN_META_TITLE=A place") {
		t.Errorf("both keys should be present:\n%s", out)
	}
	if strings.Index(out, "KBIN_META_DESC") > strings.Index(out, "KBIN_META_TITLE") {
		t.Error("keys should be sorted, so two runs render the same bytes")
	}
}

// A key the template already wrote is a collision rather than an override.
func TestEnvRefusesAKeyTheTemplateAlreadyWrote(t *testing.T) {
	_, err := configmerge.Env("DATABASE_URL=postgres://x\n", map[string]any{"DATABASE_URL": "postgres://y"})
	var collision *configmerge.Collision
	if !errors.As(err, &collision) {
		t.Fatalf("want a Collision, got %v", err)
	}
	if collision.Key != "DATABASE_URL" {
		t.Errorf("the collision should name the key, got %q", collision.Key)
	}
}

// An exported or indented assignment is still an assignment.
func TestEnvSeesThroughExportAndIndentation(t *testing.T) {
	for _, rendered := range []string{"export FOO=1\n", "  FOO=1\n", "\texport  FOO = 1\n"} {
		_, err := configmerge.Env(rendered, map[string]any{"FOO": "2"})
		var collision *configmerge.Collision
		if !errors.As(err, &collision) || collision.Key != "FOO" {
			t.Errorf("%q: want a Collision naming FOO, got %v", rendered, err)
		}
	}
	// A comment that mentions the name is not an assignment.
	if _, err := configmerge.Env("# FOO=1\n", map[string]any{"FOO": "2"}); err != nil {
		t.Errorf("a commented assignment is not a collision: %v", err)
	}
}

// The rendered file is left exactly as it was: the merge only appends.
func TestEnvKeepsTheRenderedFileByteForByte(t *testing.T) {
	rendered := readGolden(t, goldenEnv)
	out, err := configmerge.Env(rendered, map[string]any{"KBIN_META_TITLE": "A place"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, rendered) {
		t.Errorf("the rendered file, comments and blank lines included, must be a prefix of the output:\n%s", out)
	}
	if !strings.HasSuffix(out, "KBIN_META_TITLE=A place\n") {
		t.Errorf("the key belongs at the end, newline terminated:\n%s", out)
	}
	if _, err := configmerge.Env(rendered, map[string]any{"DATABASE_URL": "x"}); err == nil {
		t.Error("DATABASE_URL is in the rendered file, so setting it again is a collision")
	}
}

// Env names are refused when they are not names a shell or compose would
// read, since a key carrying `=` or a newline would write a second assignment.
func TestEnvRefusesAKeyThatIsNotAName(t *testing.T) {
	for _, key := range []string{"a.b", "A=B", "A B", "A\nB", "1ABC", ""} {
		_, err := configmerge.Env("", map[string]any{key: "x"})
		if err == nil {
			t.Errorf("%q: want an error", key)
			continue
		}
		if key != "" && !strings.Contains(err.Error(), strconv.Quote(key)) {
			t.Errorf("%q: the error should name the key: %v", key, err)
		}
	}
}

// Each line here was checked against `docker compose config` reading the
// file as an env_file; see the task report. A plain value stays unquoted, the
// style every .env template uses. Anything compose would rewrite is double
// quoted with the four escapes compose's dotenv parser undoes.
func TestEnvQuotesWhatComposeWouldOtherwiseRewrite(t *testing.T) {
	cases := []struct {
		value any
		line  string
	}{
		{"Talk here", "K=Talk here"},
		{"http://x:1/y?z=1&w=2", "K=http://x:1/y?z=1&w=2"},
		{"", "K="},
		{true, "K=true"},
		{3, "K=3"},
		{2.5, "K=2.5"},
		{"a #b", `K="a #b"`},
		{"$HOME", `K="\$HOME"`},
		{"it's", `K="it's"`},
		{`say "hi"`, `K="say \"hi\""`},
		{`a\nb`, `K="a\\nb"`},
		{"line1\nline2", `K="line1\nline2"`},
		{" lead", `K=" lead"`},
		{"trail ", `K="trail "`},
	}
	for _, c := range cases {
		out, err := configmerge.Env("", map[string]any{"K": c.value})
		if err != nil {
			t.Errorf("%q: %v", c.value, err)
			continue
		}
		if !strings.HasSuffix(out, "\n"+c.line+"\n") {
			t.Errorf("%q: want line %s, got:\n%s", c.value, c.line, out)
		}
	}
	_, err := configmerge.Env("", map[string]any{"K": "a\rb"})
	if err == nil || !strings.Contains(err.Error(), "K") {
		t.Errorf("a carriage return cannot be carried and should be refused, naming the key: %v", err)
	}
}

// Ini inserts into the section that is already there rather than appending a
// second one with the same name, because whether a repeated section merges or
// shadows is a property of whichever parser the application uses.
func TestINIInsertsIntoTheExistingSection(t *testing.T) {
	rendered := "; Rendered by paisans.\n\n[app]\n; what the site is called\nsite_name = Example\n\n[database]\ntype = postgres\n"
	out, err := configmerge.INI(rendered, map[string]any{"app.max_blogs": 3})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "[app]") != 1 {
		t.Errorf("the section must not be repeated:\n%s", out)
	}
	appBlock := out[strings.Index(out, "[app]"):strings.Index(out, "[database]")]
	if !strings.Contains(appBlock, "max_blogs = 3") {
		t.Errorf("the key belongs inside [app]:\n%s", out)
	}
	if !strings.Contains(out, "; what the site is called") {
		t.Error("comments in the rendered file must survive: the file explains itself")
	}
}

// A section the rendered file does not have is created, once.
func TestINICreatesAMissingSection(t *testing.T) {
	out, err := configmerge.INI("[app]\nsite_name = Example\n", map[string]any{"email.enabled": true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "[email]") != 1 {
		t.Errorf("want one new section:\n%s", out)
	}
	if !strings.Contains(out, "enabled = true") {
		t.Errorf("want the key:\n%s", out)
	}
}

// Against the real rendered config.ini: every line of it survives, in order,
// and the only lines added are the operator's.
func TestINIKeepsEveryLineOfTheRenderedFile(t *testing.T) {
	rendered := readGolden(t, goldenINI)
	out, err := configmerge.INI(rendered, map[string]any{
		"app.max_blogs":      3,
		"app.editor":         "pad",
		"uploads.host":       "x", // `host` is in [app] and [database], not [uploads]
		"email.smtp_enabled": true,
		"email.domain":       "example.org",
		"database.max_conns": 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	added := map[string]bool{
		iniLabel:               true,
		"max_blogs = 3":        true,
		"editor = pad":         true,
		"host = x":             true,
		"smtp_enabled = true":  true,
		"domain = example.org": true,
		"max_conns = 10":       true,
		"[email]":              true,
		"":                     true, // the blank line before a new section
	}
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if added[line] {
			continue
		}
		kept = append(kept, line)
	}
	var want []string
	for _, line := range strings.Split(rendered, "\n") {
		if line == "" {
			continue
		}
		want = append(want, line)
	}
	var keptNonBlank []string
	for _, line := range kept {
		if line != "" {
			keptNonBlank = append(keptNonBlank, line)
		}
	}
	if strings.Join(keptNonBlank, "\n") != strings.Join(want, "\n") {
		t.Errorf("every line of the rendered file must survive, in order:\n%s", out)
	}
	if strings.Count(out, "\n\n") != strings.Count(rendered, "\n\n")+1 {
		t.Errorf("the rendered file's blank lines must survive, plus one before the new section:\n%s", out)
	}
	for _, section := range []string{"[app]", "[uploads]", "[database]"} {
		if strings.Count(out, section) != 1 {
			t.Errorf("%s must not be repeated:\n%s", section, out)
		}
	}
	app := sectionBody(out, "app")
	if !strings.Contains(app, "\neditor = pad\nmax_blogs = 3\n") {
		t.Errorf("[app] should carry both keys, sorted, together:\n%s", app)
	}
	if !strings.HasSuffix(strings.TrimRight(app, "\n"), "max_blogs = 3") {
		t.Errorf("the keys go at the end of [app], after what the template wrote:\n%s", app)
	}
	if !strings.Contains(sectionBody(out, "database"), "max_conns = 10") {
		t.Errorf("[database] should carry max_conns:\n%s", out)
	}
	if !strings.Contains(sectionBody(out, "uploads"), "host = x") {
		t.Errorf("[uploads] should carry host:\n%s", out)
	}
	if !strings.HasSuffix(out, "\n\n[email]\n"+iniLabel+"\ndomain = example.org\nsmtp_enabled = true\n") {
		t.Errorf("a new section goes at the end, keys sorted:\n%s", out)
	}
}

// sectionBody is the text from a section's header to the next one.
func sectionBody(file, name string) string {
	start := strings.Index(file, "["+name+"]\n")
	if start < 0 {
		return ""
	}
	rest := file[start+len(name)+3:]
	if end := strings.Index(rest, "\n["); end >= 0 {
		return rest[:end+1]
	}
	return rest
}

// A collision is per section: the same key name in another section is fine.
func TestINIRefusesAKeyTheTemplateAlreadyWroteInThatSection(t *testing.T) {
	rendered := readGolden(t, goldenINI)
	_, err := configmerge.INI(rendered, map[string]any{"app.site_name": "other"})
	var collision *configmerge.Collision
	if !errors.As(err, &collision) || collision.Key != "app.site_name" {
		t.Fatalf("want a Collision naming app.site_name, got %v", err)
	}
	// `type` is in [database] and [storage]; [uploads] has none.
	if _, err := configmerge.INI(rendered, map[string]any{"uploads.type": "x"}); err != nil {
		t.Errorf("a key another section carries is not a collision: %v", err)
	}
}

// An ini key is exactly section.key. Anything else is an error naming it.
func TestINIRefusesAKeyThatIsNotSectionDotKey(t *testing.T) {
	for _, key := range []string{"max_blogs", "oauth.generic.client_id", ".x", "x.", "a b.c", "a.b=c", "a].b"} {
		_, err := configmerge.INI("[app]\n", map[string]any{key: "x"})
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%q: want an error naming the key, got %v", key, err)
		}
	}
}

// Values the ini syntax could misread are refused rather than guessed at.
func TestINIRefusesAValueTheSyntaxWouldMisread(t *testing.T) {
	for _, value := range []string{"a\nb", "a ; b", "a # b", " lead", "trail ", `"quoted"`, "`tick`"} {
		_, err := configmerge.INI("[app]\n", map[string]any{"app.k": value})
		if err == nil || !strings.Contains(err.Error(), "app.k") {
			t.Errorf("%q: want an error naming the key, got %v", value, err)
		}
	}
}

// Yaml nests, to any depth, and the result must parse.
func TestYAMLSetsANestedPath(t *testing.T) {
	out, err := configmerge.YAML("server_name: example.org\n", map[string]any{
		"retention.enabled":   true,
		"url_preview_enabled": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the merged file must parse: %v\n%s", err, out)
	}
	if got["server_name"] != "example.org" {
		t.Error("the rendered content must survive")
	}
	if got["url_preview_enabled"] != true {
		t.Error("a top level key should be set")
	}
	retention, ok := got["retention"].(map[string]any)
	if !ok || retention["enabled"] != true {
		t.Errorf("a dotted key should nest:\n%s", out)
	}
}

// A collision at depth is still a collision.
func TestYAMLRefusesAPathTheTemplateAlreadyWrote(t *testing.T) {
	_, err := configmerge.YAML("database:\n  name: synapse\n", map[string]any{"database.name": "other"})
	var collision *configmerge.Collision
	if !errors.As(err, &collision) {
		t.Fatalf("want a Collision, got %v", err)
	}
	// A path through a value the template wrote is a collision too: setting it
	// would replace that value with a mapping.
	_, err = configmerge.YAML("server_name: example.org\n", map[string]any{"server_name.x": 1})
	if !errors.As(err, &collision) || collision.Key != "server_name.x" {
		t.Errorf("want a Collision naming server_name.x, got %v", err)
	}
}

// Two operator keys that cannot both hold are an error, not a silent winner.
func TestYAMLRefusesKeysThatContradictEachOther(t *testing.T) {
	_, err := configmerge.YAML("a: 1\n", map[string]any{"retention": true, "retention.enabled": true})
	if err == nil || !strings.Contains(err.Error(), "retention") {
		t.Errorf("want an error naming retention, got %v", err)
	}
}

// A new top level key is appended to the rendered homeserver.yaml without
// re-emitting it, so the file is byte for byte what the template wrote, blank
// lines and comments included, with the operator's keys after it.
func TestYAMLAppendsToTheRealHomeserverWithoutTouchingIt(t *testing.T) {
	rendered := readGolden(t, goldenYAML)
	out, err := configmerge.YAML(rendered, map[string]any{
		"url_preview_enabled": true,
		"retention.enabled":   true,
		"max_upload_size":     "50M",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, rendered) {
		t.Errorf("the rendered file must be a prefix of the output:\n%s", out)
	}
	want := "\n" + yamlLabel + "\nmax_upload_size: 50M\nretention:\n  enabled: true\nurl_preview_enabled: true\n"
	if got := strings.TrimPrefix(out, rendered); got != want {
		t.Errorf("appended block:\n%s\nwant:\n%s", got, want)
	}
	var got map[string]any
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the merged file must parse: %v", err)
	}
}

// Inserting under a mapping the template wrote needs a re-emit. The comments
// survive it; that is what yaml.Node is for, and why a map is not used.
func TestYAMLKeepsTheRealHomeserversCommentsWhenInsertingUnderAMapping(t *testing.T) {
	rendered := readGolden(t, goldenYAML)
	out, err := configmerge.YAML(rendered, map[string]any{"database.args.sslmode": "disable"})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") && !strings.Contains(out, line+"\n") {
			t.Errorf("comment lost: %q", line)
		}
	}
	for _, line := range []string{
		`server_name: "example.org"`,
		"    bind_addresses: ['0.0.0.0']",
		"      - names: [client, federation]",
		"trusted_key_servers: []",
	} {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("quoting, flow style or indentation changed, missing %q:\n%s", line, out)
		}
	}
	var got struct {
		Database struct {
			Args map[string]any `yaml:"args"`
		} `yaml:"database"`
	}
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "    cp_max: 10\n    "+yamlLabel+"\n    sslmode: disable\n") {
		t.Errorf("the key goes at the end of the mapping, labelled:\n%s", out)
	}
	if got.Database.Args["sslmode"] != "disable" || got.Database.Args["cp_max"] != 10 {
		t.Errorf("want sslmode beside the rendered args: %v", got.Database.Args)
	}
}

// A string that yaml would read as something else stays a string. Synapse
// reads the file with PyYAML, which follows yaml 1.1, where a bare yes or on
// is a boolean; so those are asserted quoted, not merely read back by yaml.v3.
func TestYAMLKeepsAStringAString(t *testing.T) {
	out, err := configmerge.YAML("a: 1\n", map[string]any{"b": "yes", "c": "123", "d": "x: y", "e": "on"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got["b"] != "yes" || got["c"] != "123" || got["d"] != "x: y" || got["e"] != "on" {
		t.Errorf("want strings back: %#v\n%s", got, out)
	}
	for _, line := range []string{`b: "yes"`, `c: "123"`, `e: "on"`} {
		if !strings.Contains(out, "\n"+line+"\n") {
			t.Errorf("want %s quoted for a yaml 1.1 reader:\n%s", line, out)
		}
	}
}

// Json is the same shape as yaml and must also still parse.
func TestJSONSetsANestedPath(t *testing.T) {
	out, err := configmerge.JSON(`{"default_server_config":{"m.homeserver":{"base_url":"https://chat.example.org"}}}`, map[string]any{
		"show_labs_settings":                true,
		"setting_defaults.use_system_theme": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the merged file must parse: %v\n%s", err, out)
	}
	if got["show_labs_settings"] != true {
		t.Error("a top level key should be set")
	}
	defaults, ok := got["setting_defaults"].(map[string]any)
	if !ok || defaults["use_system_theme"] != false {
		t.Errorf("a dotted key should nest:\n%s", out)
	}
	if _, ok := got["default_server_config"]; !ok {
		t.Error("the rendered content must survive")
	}
}

// Against the real config.json: the template's key order and indentation are
// kept, so the rendered diff shows only what the operator added.
func TestJSONKeepsTheRealConfigsOrderAndIndent(t *testing.T) {
	rendered := readGolden(t, goldenJSON)
	out, err := configmerge.JSON(rendered, map[string]any{
		"setting_defaults.use_system_theme": false,
		"room_directory.servers":            "<example.org>",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(rendered,
		"    \"UIFeature.passwordReset\": false\n  }\n}\n",
		"    \"UIFeature.passwordReset\": false,\n    \"use_system_theme\": false\n  },\n  \"room_directory\": {\n    \"servers\": \"<example.org>\"\n  }\n}\n", 1)
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestJSONRefusesAPathTheTemplateAlreadyWrote(t *testing.T) {
	rendered := readGolden(t, goldenJSON)
	for _, key := range []string{"brand", "default_server_config.m.homeserver", "brand.x"} {
		_, err := configmerge.JSON(rendered, map[string]any{key: "x"})
		var collision *configmerge.Collision
		if key == "default_server_config.m.homeserver" {
			// "m.homeserver" is one key with a dot in it, which a dotted path
			// cannot name: this reaches default_server_config.m, which is new.
			if err != nil {
				t.Errorf("%s: %v", key, err)
			}
			continue
		}
		if !errors.As(err, &collision) || collision.Key != key {
			t.Errorf("%s: want a Collision naming it, got %v", key, err)
		}
	}
}

// No keys means the rendered file comes back untouched, byte for byte: the
// golden tree for every deployment without `config` must not move.
func TestNoKeysIsByteIdentical(t *testing.T) {
	for _, f := range formats {
		rendered := readGolden(t, f.golden)
		for _, keys := range []map[string]any{nil, {}} {
			out, err := f.merge(rendered, keys)
			if err != nil {
				t.Errorf("%s: %v", f.name, err)
			}
			if out != rendered {
				t.Errorf("%s: no keys must leave the file byte identical", f.name)
			}
		}
	}
}

// Nesting is expressed by the dotted key, never by a value's structure. A map
// value would also hide its inner names from validate's secret-name check,
// which reads only the top level keys.
func TestAMapOrListValueIsRefusedNamingTheKey(t *testing.T) {
	for _, f := range formats {
		for _, value := range []any{
			map[string]any{"api_token": "x"},
			[]any{"a", "b"},
			nil,
		} {
			_, err := f.merge(readGolden(t, f.golden), map[string]any{f.key: value})
			if err == nil || !strings.Contains(err.Error(), f.key) {
				t.Errorf("%s %#v: want an error naming %s, got %v", f.name, value, f.key, err)
			}
		}
	}
}

// Keys are sorted, so the same declaration always renders the same bytes,
// however Go happens to iterate the map.
func TestOutputIsDeterministic(t *testing.T) {
	keys := map[string][]string{
		"env":  {"ZZ", "AA", "MM", "BB", "YY", "CC"},
		"ini":  {"app.zz", "app.aa", "app.mm", "zz.k", "aa.k", "mm.k"},
		"yaml": {"zz", "aa.b", "mm", "aa.a", "bb.z.y", "bb.a"},
		"json": {"zz", "aa.b", "mm", "aa.a", "bb.z.y", "bb.a"},
	}
	for _, f := range formats {
		m := map[string]any{}
		for _, k := range keys[f.name] {
			m[k] = "v"
		}
		first, err := f.merge(readGolden(t, f.golden), m)
		if err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		for i := 0; i < 20; i++ {
			again, _ := f.merge(readGolden(t, f.golden), m)
			if again != first {
				t.Fatalf("%s: two runs rendered different bytes", f.name)
			}
		}
		// Sorted, not merely stable.
		last := -1
		var order []string
		switch f.name {
		case "env":
			order = []string{"AA=", "BB=", "CC=", "MM=", "YY=", "ZZ="}
		case "ini":
			order = []string{"aa =", "mm =", "zz =", "[aa]", "[mm]", "[zz]"}
		case "yaml":
			order = []string{"\naa:", "\n  a: v", "\n  b: v", "\nbb:", "\n  a: v", "\n  z:", "\nmm:", "\nzz:"}
		case "json":
			order = []string{`"aa"`, `"a": "v"`, `"b": "v"`, `"bb"`, `"a": "v"`, `"z"`, `"mm"`, `"zz"`}
		}
		for _, s := range order {
			i := strings.Index(first[last+1:], s)
			if i < 0 {
				t.Errorf("%s: %q missing or out of order:\n%s", f.name, s, first)
				break
			}
			last += 1 + i
		}
	}
}
