package apply_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/paisans-software/paisans-stack/internal/apply"
	"github.com/paisans-software/paisans-stack/internal/render"
)

// A host applied before the etcd record existed gains it on its next apply,
// and that write alone acts on no stack: no container reads the record, and
// restarting etcd and Patroni for it would be an outage for nothing.
func TestTheEtcdRecordAloneActsOnNoStack(t *testing.T) {
	host := newHost()
	rendered := plan(t)
	first, err := apply.Build("home-a", rendered, acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply.Execute(first, host); err != nil {
		t.Fatal(err)
	}
	// As an older toolkit left it: no record, and none in the manifest.
	delete(host.files, "/"+render.EtcdInitialPath)
	var m render.Manifest
	if err := json.Unmarshal([]byte(host.files["/srv/.paisans-manifest.json"]), &m); err != nil {
		t.Fatal(err)
	}
	var kept []render.ManifestFile
	for _, f := range m.Files {
		if f.Path != render.EtcdInitialPath {
			kept = append(kept, f)
		}
	}
	m.Files = kept
	data, _ := json.Marshal(m)
	host.files["/srv/.paisans-manifest.json"] = string(data)

	second, err := apply.Build("home-a", rendered, acmeModule(t), host)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Writes()) != 1 || second.Writes()[0].Path != "/"+render.EtcdInitialPath {
		t.Fatalf("want only the record written, got %v", second.Writes())
	}
	if len(second.Actions) != 0 {
		t.Errorf("writing the record acts on %v", second.Actions)
	}
}

// The record wins, and a host without one keeps the flags its compose file
// already runs with.
func TestReadEtcdInitial(t *testing.T) {
	host := newHost()
	if _, found, err := apply.ReadEtcdInitial(host); err != nil || found {
		t.Fatalf("an empty host reported a record: %v %v", found, err)
	}

	host.files["/srv/infra/compose.yaml"] = "services:\n  etcd:\n    command:\n      - --initial-cluster=home-a=http://10.44.0.1:2380\n      - --initial-cluster-state=new\n"
	in, found, err := apply.ReadEtcdInitial(host)
	if err != nil || !found || in.State != "new" || in.Cluster != "home-a=http://10.44.0.1:2380" {
		t.Fatalf("compose fallback: %+v %v %v", in, found, err)
	}

	host.files["/"+render.EtcdInitialPath] = render.FormatEtcdInitial(render.EtcdInitial{State: "existing", Cluster: "a=http://x:2380,vm=http://y:2380"})
	in, found, err = apply.ReadEtcdInitial(host)
	if err != nil || !found || in.State != "existing" || !strings.Contains(in.Cluster, "vm=") {
		t.Fatalf("record: %+v %v %v", in, found, err)
	}

	host.files["/"+render.EtcdInitialPath] = "garbage\n"
	if _, _, err := apply.ReadEtcdInitial(host); err == nil {
		t.Error("an unreadable record was accepted")
	}
}
