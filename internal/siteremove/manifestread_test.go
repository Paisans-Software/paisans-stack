package siteremove_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/render"
	"github.com/paisans-software/paisans-stack/internal/siteremove"
)

// addManifestFiles puts n more files on the host, each listed in its manifest
// with its sum, as apply would have written them.
func addManifestFiles(t *testing.T, h *host, n int) {
	t.Helper()
	var m render.Manifest
	if err := json.Unmarshal([]byte(h.files[dep.Manifest()]), &m); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		rel := strings.TrimPrefix(root, "/") + fmt.Sprintf("/extra/file-%02d.conf", i)
		content := fmt.Sprintf("extra %d\n", i)
		h.files["/"+rel] = content
		m.Files = append(m.Files, render.ManifestFile{Path: rel, SHA256: hexSum(content), Mode: "0644"})
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	h.files[dep.Manifest()] = string(data) + "\n"
}

// Reading the host costs the same number of connections however many files
// the manifest lists: one ssh connection each is over a second on a real host.
func TestTheHostReadIsBoundedWhateverTheManifestSize(t *testing.T) {
	calls := func(extra int) int {
		w := setup(t)
		b := w.hosts["home-b"]
		addManifestFiles(t, b, extra)
		w.mustBuild("home-b", siteremove.Options{})
		return b.calls()
	}
	base, big := calls(0), calls(40)
	t.Logf("calls to home-b while planning: %d with its own manifest, %d with 40 more files", base, big)
	if big != base {
		t.Errorf("40 more manifest files cost %d more calls to the host", big-base)
	}
}

// A file the manifest lists that is no longer on the host is gone: nothing to
// delete, nothing kept.
func TestAManifestFileMissingFromTheHostIsGone(t *testing.T) {
	w := setup(t)
	b := w.hosts["home-b"]
	addManifestFiles(t, b, 2)
	missing := root + "/extra/file-01.conf"
	delete(b.files, missing)
	p := w.mustBuild("home-b", siteremove.Options{})
	for _, s := range steps(p, 3) {
		if strings.Contains(s.Text, missing) {
			t.Errorf("a step names the missing file: %+v", s)
		}
	}
	if strings.Contains(strings.Join(p.Remains(), "\n"), missing) {
		t.Errorf("the report keeps the missing file:\n%s", strings.Join(p.Remains(), "\n"))
	}
	if !hasStep(p, 3, "home-b", "delete", root+"/extra/file-00.conf") {
		t.Errorf("the file still on the host is not deleted:\n%v", steps(p, 3))
	}
}

// A file whose sum no longer matches the manifest is kept, and the report
// says so.
func TestAnEditedManifestFileIsKept(t *testing.T) {
	w := setup(t)
	b := w.hosts["home-b"]
	addManifestFiles(t, b, 2)
	edited := root + "/extra/file-00.conf"
	b.files[edited] += "# a hand edit\n"
	p := w.mustBuild("home-b", siteremove.Options{})
	if !hasStep(p, 3, "home-b", "keep", edited) {
		t.Errorf("no keep step for the edited file:\n%v", steps(p, 3))
	}
	if hasStep(p, 3, "home-b", "delete", edited) {
		t.Error("the edited file is deleted")
	}
	if !strings.Contains(strings.Join(p.Remains(), "\n"), edited) {
		t.Errorf("the report does not keep the edited file:\n%s", strings.Join(p.Remains(), "\n"))
	}
}

// An edited compose file proves none of the images it names: they are not
// this deployment's to remove.
func TestAnEditedComposeFileNamesNoImages(t *testing.T) {
	w := setup(t)
	b := w.hosts["home-b"]
	compose := root + "/talk/compose.yaml"
	if _, ok := b.files[compose]; !ok {
		t.Fatalf("home-b has no %s", compose)
	}
	b.files[compose] += "# a hand edit\n"
	p := w.mustBuild("home-b", siteremove.Options{})
	if !hasStep(p, 3, "home-b", "keep", compose) {
		t.Errorf("no keep step for the edited compose file:\n%v", steps(p, 3))
	}
}

// An answer that is cut short, garbled or runs past its end stops the plan,
// naming the file it could not read, rather than taking a file as gone.
func TestABadManifestReadStopsThePlan(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer func(string) string
		want   string
	}{
		{"cut short", func(s string) string {
			lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
			return strings.Join(lines[:len(lines)-3], "\n") + "\n"
		}, "/extra/file-38.conf"},
		{"no end", func(s string) string {
			return strings.TrimSuffix(s, "end\n")
		}, "no end"},
		{"garbled", func(s string) string {
			return strings.Replace(s, "sum ", "sun ", 1)
		}, "reading /"},
		{"short sum", func(s string) string {
			i := strings.Index(s, "sum ")
			return s[:i+10] + s[i+4+64:]
		}, "reading /"},
		{"bad base64", func(s string) string {
			i := strings.Index(s, "file ")
			return s[:i+5] + "!!" + s[i+5:]
		}, "compose.yaml"},
		{"past its end", func(s string) string {
			return s + "gone\n"
		}, "after its end"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := setup(t)
			b := w.hosts["home-b"]
			addManifestFiles(t, b, 40)
			b.filesAnswer = func(s string) (string, error) { return c.answer(s), nil }
			_, err := w.build("home-b", siteremove.Options{})
			if err == nil {
				t.Fatal("the plan was built from a bad answer")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the error does not say %q: %v", c.want, err)
			}
		})
	}
}

// A file the host cannot read stops the plan, naming the file.
func TestAnUnreadableManifestFileStopsThePlan(t *testing.T) {
	w := setup(t)
	b := w.hosts["home-b"]
	addManifestFiles(t, b, 3)
	b.filesAnswer = func(s string) (string, error) {
		lines := strings.Split(s, "\n")
		return strings.Join(lines[:len(lines)-4], "\n") + "\nsh: cannot open " + root + "/extra/file-01.conf: Permission denied\n", errors.New("exit status 4")
	}
	_, err := w.build("home-b", siteremove.Options{})
	if err == nil || !strings.Contains(err.Error(), "/extra/file-01.conf") {
		t.Fatalf("the error does not name the unreadable file: %v", err)
	}
}
