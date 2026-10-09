package config

import (
	"fmt"
	"regexp"
	"strings"
)

// secretName is what a key segment chosen by an operator may look like: a
// new external secret's name, or an app's. It matches the names the rest of
// the file already uses.
var secretName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// Get returns the value at a dotted key and whether the key is set to
// anything. An unknown key is not an error here: it is simply not set.
func (s *Secrets) Get(key string) (string, bool) {
	parts := strings.Split(key, ".")
	value := ""
	switch {
	case len(parts) == 2 && parts[0] == "cluster":
		value = map[string]string{
			"superuser_password": s.Cluster.SuperuserPassword,
			"admin_password":     s.Cluster.AdminPassword,
			"standby_password":   s.Cluster.StandbyPassword,
		}[parts[1]]
	case len(parts) == 3 && parts[0] == "storage" && parts[1] == "garage":
		value = map[string]string{
			"admin_token": s.Storage.Garage.AdminToken,
			"rpc_secret":  s.Storage.Garage.RPCSecret,
		}[parts[2]]
	case len(parts) == 3 && parts[0] == "sites":
		value = map[string]string{
			"wireguard_private_key": s.Sites[parts[1]].WireGuardPrivateKey,
			"heartbeat_token":       s.Sites[parts[1]].HeartbeatToken,
		}[parts[2]]
	case len(parts) == 3 && parts[0] == "apps":
		value, _ = s.Apps[parts[1]][parts[2]].(string)
	case len(parts) == 2 && parts[0] == "external":
		value = s.External[parts[1]]
	case len(parts) == 3 && parts[0] == "oidc_clients":
		client := s.OIDCClients[parts[1]]
		value = map[string]string{"client_id": client.ClientID, "client_secret": client.ClientSecret}[parts[2]]
	}
	return value, value != ""
}

// Set writes one value at a dotted key, creating the maps on the way.
//
// It knows the file's shape and nothing about policy: which keys an operator
// may create is the caller's decision. It refuses a key that names no place in
// the file, because a value written where nothing reads it is a credential
// that looks delivered and is not.
func (s *Secrets) Set(key, value string) error {
	parts := strings.Split(key, ".")
	switch {
	case len(parts) == 2 && parts[0] == "cluster":
		switch parts[1] {
		case "superuser_password":
			s.Cluster.SuperuserPassword = value
		case "admin_password":
			s.Cluster.AdminPassword = value
		case "standby_password":
			s.Cluster.StandbyPassword = value
		default:
			return fmt.Errorf("%s: the cluster has no secret named %q", key, parts[1])
		}
	case len(parts) == 3 && parts[0] == "storage" && parts[1] == "garage":
		switch parts[2] {
		case "admin_token":
			s.Storage.Garage.AdminToken = value
		case "rpc_secret":
			s.Storage.Garage.RPCSecret = value
		default:
			return fmt.Errorf("%s: Garage has no secret named %q", key, parts[2])
		}
	case len(parts) == 3 && parts[0] == "sites" && (parts[2] == "wireguard_private_key" || parts[2] == "heartbeat_token"):
		if s.Sites == nil {
			s.Sites = map[string]SiteSecrets{}
		}
		site := s.Sites[parts[1]]
		if parts[2] == "heartbeat_token" {
			site.HeartbeatToken = value
		} else {
			site.WireGuardPrivateKey = value
		}
		s.Sites[parts[1]] = site
	case len(parts) == 3 && parts[0] == "apps" && secretName.MatchString(parts[1]) && secretName.MatchString(parts[2]):
		if s.Apps == nil {
			s.Apps = map[string]map[string]any{}
		}
		if s.Apps[parts[1]] == nil {
			s.Apps[parts[1]] = map[string]any{}
		}
		s.Apps[parts[1]][parts[2]] = value
	case len(parts) == 2 && parts[0] == "external" && secretName.MatchString(parts[1]):
		if s.External == nil {
			s.External = map[string]string{}
		}
		s.External[parts[1]] = value
	case len(parts) == 3 && parts[0] == "oidc_clients" && secretName.MatchString(parts[1]):
		if s.OIDCClients == nil {
			s.OIDCClients = map[string]OIDCClient{}
		}
		client := s.OIDCClients[parts[1]]
		switch parts[2] {
		case "client_id":
			client.ClientID = value
		case "client_secret":
			client.ClientSecret = value
		default:
			return fmt.Errorf("%s: an OIDC client has client_id and client_secret, not %q", key, parts[2])
		}
		s.OIDCClients[parts[1]] = client
	default:
		return fmt.Errorf("%s: names no secret this file holds. Secrets live under cluster, storage.garage, sites.<site>, apps.<app>, external and oidc_clients.<app>", key)
	}
	return nil
}
