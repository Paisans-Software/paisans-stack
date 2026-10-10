package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/config"
	"github.com/paisans-software/paisans-stack/internal/deployrecord"
	"github.com/paisans-software/paisans-stack/internal/ui"
)

// recordFake is a gateway that runs the deployment record's write command as
// the shell would: refused on a changed hash, else the file replaced.
type recordFake struct {
	files map[string]string
	ran   []string
	down  bool
}

var (
	recordPathRe = regexp.MustCompile(`f='([^']+)'`)
	recordSumRe  = regexp.MustCompile(`\[ "\$cur" = '([^']*)' \]`)
	recordDataRe = regexp.MustCompile(`printf %s '([^']*)' \| base64 -d`)
)

func (h *recordFake) Describe() string { return "ubuntu@vm.example.org" }

func (h *recordFake) ReadFile(p string) (string, bool, error) {
	if h.down {
		return "", false, errors.New("ssh: connect to host vm.example.org port 22: Operation timed out")
	}
	c, ok := h.files[p]
	return c, ok, nil
}

func (h *recordFake) Run(command string) (string, error) {
	if h.down {
		return "", errors.New("ssh: connect to host vm.example.org port 22: Operation timed out")
	}
	h.ran = append(h.ran, command)
	path := recordPathRe.FindStringSubmatch(command)[1]
	cur := "none"
	if c, ok := h.files[path]; ok {
		s := sha256.Sum256([]byte(c))
		cur = hex.EncodeToString(s[:])
	}
	if recordSumRe.FindStringSubmatch(command)[1] != cur {
		return deployrecord.ChangedMarker + "\n", errors.New("exit status 1")
	}
	data, _ := base64.StdEncoding.DecodeString(recordDataRe.FindStringSubmatch(command)[1])
	h.files[path] = string(data)
	return "", nil
}

func TestApplyRecordsTheDeploymentOnAGateway(t *testing.T) {
	cfg, err := config.Load(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	gw := &recordFake{files: map[string]string{}}
	if err := recordApplied(&ui.Recorder{}, cfg, "vm", gw); err != nil {
		t.Fatal(err)
	}
	rec, found, _ := deployrecord.Read(gw, cfg.Deployment())
	if !found || !rec.Lists("sites", "home-b") || !rec.Lists("apps", "talk") {
		t.Errorf("%+v %v", rec, found)
	}
	other := &recordFake{files: map[string]string{}}
	if err := recordApplied(&ui.Recorder{}, cfg, "home-b", other); err != nil || len(other.files) != 0 || len(other.ran) != 0 {
		t.Errorf("a non gateway was written: %v %v", err, other.files)
	}
}
