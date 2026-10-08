package hostcheck_test

import (
	"fmt"
	"strings"
)

// fakeHost answers each probe by a substring only that probe contains, fails
// any command it has no answer for, and records everything it was asked.
type fakeHost struct {
	answers map[string]string
	fail    map[string]error
	files   map[string]string
	ran     []string
}

func (h *fakeHost) Describe() string { return "ubuntu@host.example.org" }

func (h *fakeHost) Run(command string) (string, error) {
	h.ran = append(h.ran, command)
	for key, err := range h.fail {
		if strings.Contains(command, key) {
			return "", err
		}
	}
	for key, out := range h.answers {
		if strings.Contains(command, key) {
			return out, nil
		}
	}
	return "fake: no answer", fmt.Errorf("fake: no answer for %q", command)
}

func (h *fakeHost) ReadFile(path string) (string, bool, error) {
	content, ok := h.files[path]
	return content, ok, nil
}

// id is a container, network or volume ID: 64 of one hex digit.
func id(digit string) string { return strings.Repeat(digit, 64) }

var (
	caddyID   = id("c")
	shopID    = id("d")
	infraID   = id("e")
	talkID    = id("f")
	bridgeID  = id("1")
	hostNetID = id("2")
	noneID    = id("3")
)

// Lines of `ss -Hltnup` a stock Ubuntu server has: sshd, the stub resolver,
// chrony, and systemd-networkd's DHCP client, whose name ss truncates to 15
// characters as the kernel does.
const baseSockets = `tcp LISTEN 0 4096 0.0.0.0:22 0.0.0.0:* users:(("sshd",pid=701,fd=3))
tcp LISTEN 0 4096 [::]:22 [::]:* users:(("sshd",pid=701,fd=4))
tcp LISTEN 0 4096 127.0.0.53%lo:53 0.0.0.0:* users:(("systemd-resolve",pid=402,fd=15))
udp UNCONN 0 0 127.0.0.53%lo:53 0.0.0.0:* users:(("systemd-resolve",pid=402,fd=14))
udp UNCONN 0 0 127.0.0.1:323 0.0.0.0:* users:(("chronyd",pid=530,fd=5))
udp UNCONN 0 0 [::1]:323 [::]:* users:(("chronyd",pid=530,fd=6))
udp UNCONN 0 0 203.0.113.10%eth0:68 0.0.0.0:* users:(("systemd-network",pid=388,fd=21))
`

// Docker's three built in networks, which every install has.
var defaultNetworks = fmt.Sprintf(`{"id":%q,"name":"bridge","labels":{},"ipam":[{"Subnet":"172.17.0.0/16","Gateway":"172.17.0.1"}]}
{"id":%q,"name":"host","labels":{},"ipam":[]}
{"id":%q,"name":"none","labels":{},"ipam":null}
`, bridgeID, hostNetID, noneID)

const (
	ufwActive   = "Status: active\nLogging: on (low)\nDefault: deny (incoming), allow (outgoing), disabled (routed)\nNew profiles: skip\n"
	ufwInactive = "Status: inactive\n"
)

// cleanHost is an Ubuntu server with Docker installed and nothing run on it.
func cleanHost() *fakeHost {
	return &fakeHost{
		answers: map[string]string{
			"id -u":                  "0\n",
			"docker version":         "27.3.1\n",
			"dpkg-query":             "docker-ce install ok installed\n",
			"docker inspect":         "",
			"docker volume inspect":  "",
			"docker network inspect": defaultNetworks,
			"ss -Hltnup":             baseSockets,
			"/proc/":                 "701 0::/system.slice/ssh.service \n402 0::/system.slice/systemd-resolved.service \n",
			"ip -o link":             "1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN mode DEFAULT group default qlen 1000\\    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00\n2: eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP mode DEFAULT group default qlen 1000\\    link/ether 02:00:00:00:00:01 brd ff:ff:ff:ff:ff:ff\n3: docker0: <NO-CARRIER,BROADCAST,MULTICAST,UP> mtu 1500 qdisc noqueue state DOWN mode DEFAULT group default\\    link/ether 02:00:00:00:00:02 brd ff:ff:ff:ff:ff:ff\n",
			"ip -j route":            `[{"dst":"default","gateway":"203.0.113.1","dev":"eth0"},{"dst":"203.0.113.0/24","dev":"eth0"},{"dst":"172.17.0.0/16","dev":"docker0"}]`,
			"ufw status verbose":     ufwInactive,
			"is-active firewalld":    "inactive\n",
			"/srv/caddy.d/":          "",
		},
		files: map[string]string{},
	}
}

// caddyHost is a shared host: somebody's Caddy in host networking on 80 and
// 443, and an unrelated shop container publishing one port, each in its own
// compose project, with ufw already up and denying by default.
func caddyHost() *fakeHost {
	h := cleanHost()
	h.answers["ufw status verbose"] = ufwActive
	h.answers["docker inspect"] = fmt.Sprintf(`{"id":%q,"name":"/web-caddy-1","pid":812,"labels":{"com.docker.compose.project":"web","com.docker.compose.service":"caddy"},"ports":{}}
{"id":%q,"name":"/shop-app-1","pid":913,"labels":{"com.docker.compose.project":"shop"},"ports":{"8080/tcp":[{"HostIp":"203.0.113.10","HostPort":"8081"}]}}
`, caddyID, shopID)
	h.answers["docker volume inspect"] = `{"name":"shop_data","labels":{"com.docker.compose.project":"shop","com.docker.compose.volume":"data"}}
`
	h.answers["docker network inspect"] = defaultNetworks + fmt.Sprintf(`{"id":%q,"name":"shop_default","labels":{"com.docker.compose.project":"shop"},"ipam":[{"Subnet":"192.0.2.0/24","Gateway":"192.0.2.1"}]}
`, id("4"))
	h.answers["ss -Hltnup"] = baseSockets + `tcp LISTEN 0 4096 *:80 *:* users:(("caddy",pid=812,fd=7))
tcp LISTEN 0 4096 *:443 *:* users:(("caddy",pid=812,fd=8))
udp UNCONN 0 0 *:443 *:* users:(("caddy",pid=812,fd=9))
tcp LISTEN 0 4096 203.0.113.10:8081 0.0.0.0:* users:(("docker-proxy",pid=1001,fd=4))
`
	h.answers["/proc/"] = "812 0::/system.slice/docker-" + caddyID + ".scope \n1001 0::/system.slice/docker.service \n"
	return h
}
