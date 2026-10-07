package apply

import (
	"fmt"

	"github.com/paisans-software/paisans-stack/internal/render"
)

// ReadEtcdInitial returns the flags this host's etcd member was first started
// with, for render.WithEtcdInitial. found is false on a host with no etcd
// member yet, which renders as a founder.
//
// The record is read first. A host deployed before the record existed has
// none, and its compose file's own flags are the answer instead: they are
// what the member runs with, so repeating them changes nothing. Falling back
// to etcd.members there would be wrong the moment the configuration names
// more members than the cluster was born with, and a compose file that
// changed only in inert flags would still make apply recreate the member.
func ReadEtcdInitial(t Transport) (render.EtcdInitial, bool, error) {
	content, found, err := t.ReadFile(remoteRoot + render.EtcdInitialPath)
	if err != nil {
		return render.EtcdInitial{}, false, err
	}
	if found {
		in, ok := render.ParseEtcdInitial(content)
		if !ok {
			return render.EtcdInitial{}, false, fmt.Errorf("%s%s holds no initial-cluster-state and initial-cluster lines. It records how this etcd member was first started; restore it from the member's compose file, or delete it to fall back to that file", remoteRoot, render.EtcdInitialPath)
		}
		return in, true, nil
	}
	compose, found, err := t.ReadFile(remoteRoot + gatewayCompose)
	if err != nil || !found {
		return render.EtcdInitial{}, false, err
	}
	in, ok := render.ParseEtcdFlags(compose)
	return in, ok, nil
}
