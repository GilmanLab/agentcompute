//go:build integration

package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/meigma/codemode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GilmanLab/agentcompute/internal/compute"
	"github.com/GilmanLab/agentcompute/internal/incus"
)

var phase5Capabilities = []string{
	"sandbox.create", "sandbox.list", "sandbox.get", "sandbox.extend", "sandbox.delete",
	"image.list",
	"instance.create", "instance.list", "instance.get", "instance.delete", "instance.exec",
	"instance.start", "instance.stop", "instance.restart", "instance.wait",
	"instance.file.read", "instance.file.write",
	"instance.snapshot.create", "instance.snapshot.restore", "instance.snapshot.delete", "instance.snapshot.list",
	"instance.publish",
	"net.create", "net.list", "net.get", "net.delete", "net.attach", "net.detach",
	"net.peer", "net.acl.add", "net.acl.remove", "net.forward", "net.impair",
}

type ovnFixture struct {
	Remote     string
	Members    []string
	MgmtProbe  string
	OOBProbe   string
	Uplink     string
	Ranges     string
	RouterNAT  string
	Northbound string
}

// TestOVNAcceptance exercises the production binary against a live OVN sandbox.
//
// It does not start, stop, or reconfigure OVN central. Central outage proof is
// a separate fleet orchestration. Required infrastructure: Incus 7.4 cluster
// with chassis, TLS NB/SB, physical uplink fast40-uplink, VLAN 40 OVN range
// 10.10.40.64-10.10.40.127, catalog image "router", and operator reachability
// to VLAN 40 forwards. Never recreate the deleted default/soak01 macvlan fixture.
func TestOVNAcceptance(t *testing.T) {
	fx := loadOVNFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	t.Cleanup(cancel)
	root := integrationRoot(t)
	dir := t.TempDir()
	binary := buildAgentcompute(ctx, t, root, dir)
	config := writeOVNConfig(t, dir, root, fx)
	backend, err := incus.New(ctx, incus.Options{
		Remote:    fx.Remote,
		Pool:      "data",
		OVNUplink: fx.Uplink,
		OVNRanges: fx.Ranges,
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, backend.Close()) })

	session, _ := startClusterClient(ctx, t, binary, config, phase5Capabilities...)
	run := func(source string) map[string]any {
		return integrationExecute(ctx, t, session, source)
	}
	fail := func(source string) {
		integrationExecuteError(ctx, t, session, source)
	}

	created := run(`def main():
    return sandbox.create(ttl_minutes=30)
`)
	name := asString(t, created["name"])
	require.Equal(t, "ovn", asMap(t, created["network"])["kind"])
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		if _, getErr := backend.GetSandbox(cleanup, name); getErr == nil {
			assert.NoError(t, backend.DeleteSandbox(cleanup, name))
		}
	})

	listed := run(fmt.Sprintf(`def main():
    return {"listed": net.list(sandbox=%q), "default": net.get(sandbox=%q, name="default")}
`, name, name))
	require.NotEmpty(t, asSlice(t, asMap(t, listed["listed"])["items"]))
	require.Equal(t, "ovn", asMap(t, listed["default"])["kind"])
	require.Equal(t, "default", asMap(t, listed["default"])["name"])

	require.GreaterOrEqual(t, len(fx.Members), 2)
	memberA, memberB := fx.Members[0], fx.Members[1]
	guests := run(fmt.Sprintf(`def main():
    a = instance.create(sandbox=%q, name="a", image="router", host=%q)
    b = instance.create(sandbox=%q, name="b", image="router", host=%q)
    instance.wait(sandbox=%q, name="a", until="agent", timeout_seconds=120)
    instance.wait(sandbox=%q, name="b", until="agent", timeout_seconds=120)
    instance.wait(sandbox=%q, name="a", until="network", timeout_seconds=120)
    instance.wait(sandbox=%q, name="b", until="network", timeout_seconds=120)
    return {"a": instance.get(sandbox=%q, name="a"), "b": instance.get(sandbox=%q, name="b"), "host_a": a["host"], "host_b": b["host"]}
`, name, memberA, name, memberB, name, name, name, name, name, name))
	gotA := asMap(t, guests["a"])
	gotB := asMap(t, guests["b"])
	require.Equal(t, memberA, asString(t, guests["host_a"]))
	require.Equal(t, memberB, asString(t, guests["host_b"]))
	require.NotEqual(t, asString(t, guests["host_a"]), asString(t, guests["host_b"]))
	require.Len(t, asSlice(t, gotA["nics"]), 1)
	ipB := firstGlobalIPv4(t, gotB)
	ping := run(fmt.Sprintf(`def main():
    return instance.exec(sandbox=%q, name="a", command="ping -c 3 -W 2 %s")
`, name, ipB))
	require.Zero(t, jsonInt(t, ping["exit_code"]))
	require.False(t, jsonBool(t, ping["timed_out"]))

	mgmtHost, mgmtPort := splitProbe(t, fx.MgmtProbe)
	oobHost, oobPort := splitProbe(t, fx.OOBProbe)
	probes := run(fmt.Sprintf(`def main():
    which = instance.exec(sandbox=%q, name="a", command="command -v nc")
    mgmt = instance.exec(sandbox=%q, name="a", command="nc -z -w 3 %s %s", timeout_seconds=10)
    oob = instance.exec(sandbox=%q, name="a", command="nc -z -w 3 %s %s", timeout_seconds=10)
    return {"which": which, "mgmt": mgmt, "oob": oob}
`, name, name, mgmtHost, mgmtPort, name, oobHost, oobPort))
	require.Zero(
		t,
		jsonInt(t, asMap(t, probes["which"])["exit_code"]),
		"router image has no nc; cannot probe baseline ACL",
	)
	require.False(t, execSucceeded(t, asMap(t, probes["mgmt"])), "management probe must fail closed")
	require.False(t, execSucceeded(t, asMap(t, probes["oob"])), "OOB probe must fail closed")

	for _, rule := range []string{
		"baseline-egress-mgmt",
		"baseline-egress-oob",
	} {
		fail(fmt.Sprintf(`def main():
    return net.acl.remove(sandbox=%q, network="default", rule=%q)
`, name, rule))
	}
	fail(fmt.Sprintf(`def main():
    return net.acl.add(sandbox=%q, network="default", direction="egress", action="allow", dst="10.10.10.0/24")
`, name))
	fail(fmt.Sprintf(`def main():
    return net.acl.add(sandbox=%q, network="default", direction="egress", action="allow", dst="10.10.70.0/24")
`, name))
	still := run(fmt.Sprintf(`def main():
    mgmt = instance.exec(sandbox=%q, name="a", command="nc -z -w 3 %s %s", timeout_seconds=10)
    oob = instance.exec(sandbox=%q, name="a", command="nc -z -w 3 %s %s", timeout_seconds=10)
    return {"mgmt": mgmt, "oob": oob}
`, name, mgmtHost, mgmtPort, name, oobHost, oobPort))
	require.False(t, execSucceeded(t, asMap(t, still["mgmt"])), "allow must not bypass management baseline")
	require.False(t, execSucceeded(t, asMap(t, still["oob"])), "allow must not bypass OOB baseline")

	files := run(fmt.Sprintf(`def main():
    written = instance.file.write(sandbox=%q, name="a", path="/tmp/agentcompute-mode", content="hello-mode", mode="0640")
    mode = instance.exec(sandbox=%q, name="a", command="stat -c %%a /tmp/agentcompute-mode")
    instance.exec(sandbox=%q, name="a", command="head -c 200000 /dev/zero | tr '\\000' a > /tmp/agentcompute-big")
    default_read = instance.file.read(sandbox=%q, name="a", path="/tmp/agentcompute-big")
    capped = instance.file.read(sandbox=%q, name="a", path="/tmp/agentcompute-big", max_bytes=1000)
    return {"written": written, "mode": mode, "default_read": default_read, "capped": capped}
`, name, name, name, name, name))
	require.Equal(t, int64(len("hello-mode")), jsonInt(t, asMap(t, files["written"])["bytes"]))
	require.Zero(t, jsonInt(t, asMap(t, files["mode"])["exit_code"]))
	require.Equal(t, "640", strings.TrimSpace(asString(t, asMap(t, files["mode"])["stdout"])))
	require.True(t, jsonBool(t, asMap(t, files["default_read"])["truncated"]))
	require.Len(t, asString(t, asMap(t, files["default_read"])["content"]), 64*1024)
	require.True(t, jsonBool(t, asMap(t, files["capped"])["truncated"]))
	require.Len(t, asString(t, asMap(t, files["capped"])["content"]), 1000)
	fail(fmt.Sprintf(`def main():
    return instance.file.write(sandbox=%q, name="a", path="/tmp/agentcompute-too-big", content="a"*65537)
`, name))

	snaps := run(fmt.Sprintf(`def main():
    instance.file.write(sandbox=%q, name="a", path="/tmp/agentcompute-undo", content="before")
    instance.snapshot.create(sandbox=%q, name="a", snapshot="undo")
    listed = instance.snapshot.list(sandbox=%q, name="a")
    instance.file.write(sandbox=%q, name="a", path="/tmp/agentcompute-undo", content="after")
    after = instance.file.read(sandbox=%q, name="a", path="/tmp/agentcompute-undo")
    instance.snapshot.restore(sandbox=%q, name="a", snapshot="undo")
    restored = instance.file.read(sandbox=%q, name="a", path="/tmp/agentcompute-undo")
    instance.snapshot.create(sandbox=%q, name="a", snapshot="fresh")
    instance.snapshot.delete(sandbox=%q, name="a", snapshot="fresh")
    remaining = instance.snapshot.list(sandbox=%q, name="a")
    return {"after": after, "restored": restored, "listed": listed, "remaining": remaining}
`, name, name, name, name, name, name, name, name, name, name))
	require.Equal(t, "after", asString(t, asMap(t, snaps["after"])["content"]))
	require.Equal(t, "before", asString(t, asMap(t, snaps["restored"])["content"]))
	items := asSlice(t, asMap(t, snaps["listed"])["items"])
	require.NotEmpty(t, items)
	foundUndo := false
	for _, raw := range items {
		item := asMap(t, raw)
		if asString(t, item["name"]) == "undo" {
			foundUndo = true
			_, parseErr := time.Parse(time.RFC3339, asString(t, item["created_at"]))
			require.NoError(t, parseErr)
		}
	}
	require.True(t, foundUndo)
	require.Empty(t, asSlice(t, asMap(t, snaps["remaining"])["items"]))

	power := run(fmt.Sprintf(`def main():
    stopped = instance.stop(sandbox=%q, name="a")
    waited = instance.wait(sandbox=%q, name="a", until="stopped", timeout_seconds=120)
    started = instance.start(sandbox=%q, name="a")
    running = instance.wait(sandbox=%q, name="a", until="running", timeout_seconds=120)
    restarted = instance.restart(sandbox=%q, name="a")
    agent = instance.wait(sandbox=%q, name="a", until="agent", timeout_seconds=120)
    return {"stopped": stopped, "waited": waited, "started": started, "running": running, "restarted": restarted, "agent": agent}
`, name, name, name, name, name, name))
	require.NotEmpty(t, asString(t, asMap(t, power["waited"])["status"]))
	require.NotEmpty(t, asString(t, asMap(t, power["running"])["status"]))
	require.GreaterOrEqual(t, jsonInt(t, asMap(t, power["waited"])["elapsed_seconds"]), int64(0))
	fail(fmt.Sprintf(`def main():
    return instance.wait(sandbox=%q, name="a", until="desktop", timeout_seconds=5)
`, name))

	attached := run(fmt.Sprintf(`def main():
    spare = net.create(sandbox=%q, name="spare", cidr="192.168.82.0/24", nat=False)
    nic = net.attach(sandbox=%q, instance="a", network="spare")
    return {"spare": spare, "nic": nic, "a": instance.get(sandbox=%q, name="a")}
`, name, name, name))
	spareNIC := asString(t, asMap(t, attached["nic"])["nic"])
	require.NotEmpty(t, spareNIC)
	require.True(t, hasNIC(t, asMap(t, attached["a"]), spareNIC))
	require.Contains(t, nicNetworks(t, asMap(t, attached["a"])), "spare")
	require.Contains(t, nicNetworks(t, asMap(t, attached["a"])), "default")
	isolated, _, err := backend.Scoped(ctx, "ac-"+name, "").GetNetwork("spare")
	require.NoError(t, err)
	require.Empty(
		t,
		isolated.Config["volatile.network.ipv4.address"],
		"isolated networks must not consume uplink addresses",
	)
	fail(fmt.Sprintf(`def main():
    return net.forward(sandbox=%q, network="spare", instance="a", port=8080)
`, name))
	detached := run(fmt.Sprintf(`def main():
    net.detach(sandbox=%q, instance="a", nic=%q)
    return {"a": instance.get(sandbox=%q, name="a")}
`, name, spareNIC, name))
	require.False(t, hasNIC(t, asMap(t, detached["a"]), spareNIC))
	require.NotContains(t, nicNetworks(t, asMap(t, detached["a"])), "spare")
	require.Equal(t, []string{"default"}, nicNetworks(t, asMap(t, detached["a"])))
	require.Len(t, asSlice(t, asMap(t, detached["a"])["nics"]), 1)

	peer := run(fmt.Sprintf(`def main():
    east = net.create(sandbox=%q, name="east", cidr="192.168.80.0/24", nat=False)
    west = net.create(sandbox=%q, name="west", cidr="192.168.81.0/24", nat=False)
    net.peer(sandbox=%q, network="east", peer="west")
    peera = instance.create(sandbox=%q, name="peera", image="router", network="east", host=%q)
    peerb = instance.create(sandbox=%q, name="peerb", image="router", network="west", host=%q)
    instance.wait(sandbox=%q, name="peera", until="agent", timeout_seconds=120)
    instance.wait(sandbox=%q, name="peerb", until="agent", timeout_seconds=120)
    instance.wait(sandbox=%q, name="peera", until="network", timeout_seconds=120)
    instance.wait(sandbox=%q, name="peerb", until="network", timeout_seconds=120)
    return {"east": east, "west": west, "host_a": peera["host"], "host_b": peerb["host"], "peera": instance.get(sandbox=%q, name="peera"), "peerb": instance.get(sandbox=%q, name="peerb")}
`, name, name, name, name, memberA, name, memberB, name, name, name, name, name, name))
	require.Equal(t, "ovn", asMap(t, peer["east"])["kind"])
	require.Equal(t, "ovn", asMap(t, peer["west"])["kind"])
	require.Equal(t, memberA, asString(t, peer["host_a"]))
	require.Equal(t, memberB, asString(t, peer["host_b"]))
	require.NotEqual(t, asString(t, peer["host_a"]), asString(t, peer["host_b"]))
	gotPeerA := asMap(t, peer["peera"])
	gotPeerB := asMap(t, peer["peerb"])
	require.Equal(t, []string{"east"}, nicNetworks(t, gotPeerA))
	require.Equal(t, []string{"west"}, nicNetworks(t, gotPeerB))
	require.Len(t, asSlice(t, gotPeerA["nics"]), 1)
	require.Len(t, asSlice(t, gotPeerB["nics"]), 1)
	peerANIC := asString(t, asMap(t, asSlice(t, gotPeerA["nics"])[0])["name"])
	peerBNIC := asString(t, asMap(t, asSlice(t, gotPeerB["nics"])[0])["name"])
	peerBIP := nicIPv4(t, gotPeerB, peerBNIC)
	peerPing := run(fmt.Sprintf(`def main():
    return instance.exec(sandbox=%q, name="peera", command="ping -c 3 -W 2 %s")
`, name, peerBIP))
	require.True(t, execSucceeded(t, peerPing), "east/west peer ping from peera to %s: %#v", peerBIP, peerPing)
	isolation := run(fmt.Sprintf(`def main():
    return instance.exec(sandbox=%q, name="peera", command="ping -c 2 -W 2 10.10.40.1", timeout_seconds=10)
`, name))
	require.False(t, execSucceeded(t, isolation), "isolated peered networks must not reach the lab uplink")

	acl := run(fmt.Sprintf(`def main():
    drop = net.acl.add(sandbox=%q, network="east", direction="egress", action="drop", protocol="icmp")
    blocked = instance.exec(sandbox=%q, name="peera", command="ping -c 3 -W 2 %s", timeout_seconds=15)
    still = instance.exec(sandbox=%q, name="a", command="ping -c 3 -W 2 %s")
    return {"rule": drop["rule"], "blocked": blocked, "still": still}
`, name, name, peerBIP, name, ipB))
	require.NotEmpty(t, asString(t, acl["rule"]))
	require.False(t, execSucceeded(t, asMap(t, acl["blocked"])), "east ICMP drop must block peera->peerb")
	require.True(t, execSucceeded(t, asMap(t, acl["still"])), "default a->b must stay reachable")
	recovered := run(fmt.Sprintf(`def main():
    net.acl.remove(sandbox=%q, network="east", rule=%q)
    return instance.exec(sandbox=%q, name="peera", command="ping -c 3 -W 2 %s")
`, name, asString(t, acl["rule"]), name, peerBIP))
	require.True(t, execSucceeded(t, recovered), "peer ping must recover after east ICMP drop is removed")

	baselinePing := run(fmt.Sprintf(`def main():
    return instance.exec(sandbox=%q, name="peera", command="ping -c 5 -W 2 %s")
`, name, peerBIP))
	require.True(t, execSucceeded(t, baselinePing), "unimpaired peer ping: %#v", baselinePing)
	baselineRTT := pingAvgMS(t, asString(t, baselinePing["stdout"]))
	require.Less(
		t,
		baselineRTT,
		80.0,
		"unimpaired overlay ping must stay well under 200ms netem, rtt=%.3f",
		baselineRTT,
	)
	delayed := run(fmt.Sprintf(`def main():
    net.impair(sandbox=%q, instance="peera", nic=%q, latency_ms=200)
    pinged = instance.exec(sandbox=%q, name="peera", command="ping -c 5 -W 2 %s", timeout_seconds=30)
    qdisc = instance.exec(sandbox=%q, name="peera", command="tc qdisc show dev %s")
    return {"ping": pinged, "qdisc": qdisc}
`, name, peerANIC, name, peerBIP, name, peerANIC))
	require.True(t, execSucceeded(t, asMap(t, delayed["ping"])), "impaired ping: %#v", delayed["ping"])
	delayedRTT := pingAvgMS(t, asString(t, asMap(t, delayed["ping"])["stdout"]))
	require.Greater(
		t,
		delayedRTT,
		baselineRTT+100,
		"latency_ms=200 must raise ping RTT (baseline=%.3f delayed=%.3f)",
		baselineRTT,
		delayedRTT,
	)
	require.Zero(t, jsonInt(t, asMap(t, delayed["qdisc"])["exit_code"]))
	require.Contains(t, asString(t, asMap(t, delayed["qdisc"])["stdout"]), "qdisc netem 1:")
	rated := run(fmt.Sprintf(`def main():
    net.impair(sandbox=%q, instance="peera", nic=%q, rate_mbit=10)
    qdisc = instance.exec(sandbox=%q, name="peera", command="tc qdisc show dev %s")
    return qdisc
`, name, peerANIC, name, peerANIC))
	require.Zero(t, jsonInt(t, rated["exit_code"]))
	rateOut := strings.ToLower(asString(t, rated["stdout"]))
	require.Contains(t, rateOut, "qdisc netem 1:")
	require.Contains(t, rateOut, "rate")
	require.Contains(t, rateOut, "10mbit")
	cleared := run(fmt.Sprintf(`def main():
    net.impair(sandbox=%q, instance="peera", nic=%q, clear=True)
    qdisc = instance.exec(sandbox=%q, name="peera", command="tc qdisc show dev %s")
    pinged = instance.exec(sandbox=%q, name="peera", command="ping -c 5 -W 2 %s", timeout_seconds=30)
    return {"qdisc": qdisc, "ping": pinged}
`, name, peerANIC, name, peerANIC, name, peerBIP))
	require.Zero(t, jsonInt(t, asMap(t, cleared["qdisc"])["exit_code"]))
	require.NotContains(t, asString(t, asMap(t, cleared["qdisc"])["stdout"]), "qdisc netem 1:")
	require.True(t, execSucceeded(t, asMap(t, cleared["ping"])), "cleared ping: %#v", cleared["ping"])
	clearedRTT := pingAvgMS(t, asString(t, asMap(t, cleared["ping"])["stdout"]))
	require.Less(
		t,
		clearedRTT,
		baselineRTT+50,
		"clear must remove netem latency (baseline=%.3f cleared=%.3f)",
		baselineRTT,
		clearedRTT,
	)

	topo := run(fmt.Sprintf(`def main():
    lan = net.create(sandbox=%q, name="lan", cidr="192.168.50.0/24", nat=False)
    wan = net.create(sandbox=%q, name="wan", cidr="10.99.0.0/24", nat=True)
    rtr = instance.create(sandbox=%q, name="rtr", image="router", network="lan")
    wan_nic = net.attach(sandbox=%q, instance="rtr", network="wan")
    client = instance.create(sandbox=%q, name="client", image="ubuntu/24.04", network="lan")
    instance.wait(sandbox=%q, name="rtr", until="agent", timeout_seconds=120)
    instance.wait(sandbox=%q, name="rtr", until="network", timeout_seconds=120)
    nat_path = instance.exec(sandbox=%q, name="rtr", command="test -e %s")
    return {"lan": lan, "wan": wan, "rtr": instance.get(sandbox=%q, name="rtr"), "client": instance.get(sandbox=%q, name="client"), "wan_nic": wan_nic, "nat_path": nat_path}
`, name, name, name, name, name, name, name, name, fx.RouterNAT, name, name))
	require.Zero(t, jsonInt(t, asMap(t, topo["nat_path"])["exit_code"]), "router image must ship %s", fx.RouterNAT)
	nat := run(fmt.Sprintf(`def main():
    return instance.exec(sandbox=%q, name="rtr", command="%s --mode port-restricted --inside eth0 --outside eth1")
`, name, fx.RouterNAT))
	require.Zero(t, jsonInt(t, nat["exit_code"]))
	rtrLAN := nicIPv4(t, asMap(t, topo["rtr"]), "eth0")
	routed := run(fmt.Sprintf(`def main():
    instance.wait(sandbox=%q, name="client", until="network", timeout_seconds=120)
    return instance.exec(sandbox=%q, name="client", command="ip route replace 10.99.0.0/24 via %s && ping -c 3 -W 2 10.99.0.1")
`, name, name, rtrLAN))
	require.Zero(t, jsonInt(t, routed["exit_code"]), "client must reach WAN through the router instance: %#v", routed)
	clientNICs := asSlice(t, asMap(t, topo["client"])["nics"])
	require.Len(t, clientNICs, 1)
	require.Equal(t, "lan", asMap(t, clientNICs[0])["network"])
	rtrNetworks := nicNetworks(t, asMap(t, topo["rtr"]))
	require.Contains(t, rtrNetworks, "lan")
	require.Contains(t, rtrNetworks, "wan")
	require.NotContains(t, rtrNetworks, "default")
	script := "#!/bin/sh\nwhile true; do printf 'HTTP/1.1 200 OK\\r\\nContent-Length: 10\\r\\nConnection: close\\r\\n\\r\\nforward-ok' | nc -l -p 8080; done\n"
	listener := run(fmt.Sprintf(`def main():
    written = instance.file.write(sandbox=%q, name="a", path="/usr/local/bin/forward-ok", content=%q, mode="0755")
    started = instance.exec(sandbox=%q, name="a", command="start-stop-daemon -S -b -m -p /run/forward-ok.pid -x /usr/local/bin/forward-ok")
    return {"written": written, "started": started}
`, name, script, name))
	require.Zero(t, jsonInt(t, asMap(t, listener["started"])["exit_code"]))
	time.Sleep(time.Second)
	fwd := run(fmt.Sprintf(`def main():
    return net.forward(sandbox=%q, network="default", instance="a", port=8080)
`, name))
	address := asString(t, fwd["address"])
	port := jsonInt(t, fwd["port"])
	require.NotEmpty(t, address)
	require.Equal(t, int64(8080), port)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(address, fmt.Sprintf("%d", port)) + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "forward-ok", string(body))

	fail(fmt.Sprintf(`def main():
    return instance.publish(sandbox=%q, name="a", image="published")
`, name))
	published := run(fmt.Sprintf(`def main():
    instance.file.write(sandbox=%q, name="a", path="/tmp/agentcompute-published", content="from-sandbox-image")
    instance.stop(sandbox=%q, name="a")
    instance.wait(sandbox=%q, name="a", until="stopped", timeout_seconds=120)
    image = instance.publish(sandbox=%q, name="a", image="published")
    clone = instance.create(sandbox=%q, name="clone", image="published")
    marker = instance.file.read(sandbox=%q, name="clone", path="/tmp/agentcompute-published")
    return {"image": image, "clone": clone, "marker": marker}
`, name, name, name, name, name, name))
	require.Equal(t, "published", asString(t, asMap(t, published["image"])["image"]))
	require.Equal(t, "from-sandbox-image", asString(t, asMap(t, published["marker"])["content"]))
	alias, _, aliasErr := backend.Scoped(ctx, "ac-"+name, "").GetImageAlias("published")
	require.NoError(t, aliasErr)
	require.NotNil(t, alias)
	require.NotEmpty(t, alias.Target)

	owned, err := backend.ListNetworks(ctx, name)
	require.NoError(t, err)
	var physical []string
	for _, network := range owned {
		if network.Kind == "bridge" && network.PhysicalName != "" {
			physical = append(physical, network.PhysicalName)
		}
	}

	run(fmt.Sprintf(`def main():
    sandbox.delete(name=%q)
    return {"deleted": True}
`, name))
	assertNoSandboxResidue(t, ctx, backend, name, physical, fx.Members, address)
}

func TestOVNNetworkCreateRejectsIsolationChange(t *testing.T) {
	fx := loadOVNFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	backend, err := incus.New(ctx, incus.Options{Remote: fx.Remote, Pool: "data"})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, backend.Close()) })
	name := fmt.Sprintf("isolation-%x", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		assert.NoError(t, backend.DeleteSandbox(cleanup, name))
	})
	require.NoError(t, backend.CreateSandbox(ctx, compute.Sandbox{
		Name: name, NetworkKind: "ovn", Subject: "local",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}))
	original, err := backend.GetNetwork(ctx, name, "default")
	require.NoError(t, err)
	require.True(t, original.NAT)
	isolated := original
	isolated.NAT = false
	_, err = backend.CreateNetwork(ctx, name, isolated)
	var conflict *codemode.AgentError
	require.ErrorAs(t, err, &conflict)
	unchanged, err := backend.GetNetwork(ctx, name, "default")
	require.NoError(t, err)
	require.True(t, unchanged.NAT, "create must not convert an existing network")
	reused, err := backend.CreateNetwork(ctx, name, original)
	require.NoError(t, err)
	require.Equal(t, original.CIDR, reused.CIDR)
}

func loadOVNFixture(t *testing.T) ovnFixture {
	t.Helper()
	fx := ovnFixture{
		Remote:     requireTestRemote(t),
		Members:    csvEnv("AGENTCOMPUTE_TEST_MEMBERS", "lab01,lab03"),
		MgmtProbe:  envOr("AGENTCOMPUTE_TEST_MGMT_PROBE", "10.10.10.14:8443"),
		OOBProbe:   envOr("AGENTCOMPUTE_TEST_OOB_PROBE", "10.10.70.20:443"),
		Uplink:     os.Getenv("AGENTCOMPUTE_TEST_OVN_UPLINK"),
		Ranges:     os.Getenv("AGENTCOMPUTE_TEST_OVN_RANGES"),
		RouterNAT:  envOr("AGENTCOMPUTE_TEST_ROUTER_NAT", "/opt/router/nat"),
		Northbound: os.Getenv("AGENTCOMPUTE_TEST_OVN_NORTHBOUND"),
	}
	t.Logf(
		"OVN fixture remote=%s members=%v mgmt=%s oob=%s uplink=%q ranges=%q router_nat=%s northbound=%q (inspect-only; tests do not mutate central)",
		fx.Remote,
		fx.Members,
		fx.MgmtProbe,
		fx.OOBProbe,
		fx.Uplink,
		fx.Ranges,
		fx.RouterNAT,
		fx.Northbound,
	)
	return fx
}

func writeOVNConfig(t *testing.T, dir, root string, fx ovnFixture) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "incus:\n  remote: %q\n  pool: data\n", fx.Remote)
	if fx.Uplink != "" {
		fmt.Fprintf(&b, "  ovn_uplink: %q\n", fx.Uplink)
	}
	if fx.Ranges != "" {
		fmt.Fprintf(&b, "  ovn_ranges: %q\n", fx.Ranges)
	}
	b.WriteString("sandbox:\n  default_network_kind: ovn\n  default_ttl_minutes: 30\n")
	fmt.Fprintf(&b, "images_file: %q\n", filepath.Join(root, "images", "catalog.yaml"))
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o600))
	return path
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func csvEnv(key, fallback string) []string {
	var out []string
	for _, part := range strings.Split(envOr(key, fallback), ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func splitProbe(t *testing.T, value string) (string, string) {
	t.Helper()
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		if ip := net.ParseIP(value); ip != nil {
			return value, "22"
		}
		require.NoError(t, err)
	}
	return host, port
}

func execSucceeded(t *testing.T, result map[string]any) bool {
	t.Helper()
	return jsonInt(t, result["exit_code"]) == 0 && !jsonBool(t, result["timed_out"])
}

func firstGlobalIPv4(t *testing.T, instance map[string]any) string {
	t.Helper()
	for _, raw := range asSlice(t, instance["nics"]) {
		nic := asMap(t, raw)
		addrs, _ := nic["addresses"].([]any)
		for _, addr := range addrs {
			s, _ := addr.(string)
			if ip := globalIPv4(s); ip != "" {
				return ip
			}
		}
	}
	t.Fatalf("no global IPv4 on instance: %#v", instance)
	return ""
}

func nicIPv4(t *testing.T, instance map[string]any, nicName string) string {
	t.Helper()
	for _, raw := range asSlice(t, instance["nics"]) {
		nic := asMap(t, raw)
		if asString(t, nic["name"]) != nicName {
			continue
		}
		addrs, _ := nic["addresses"].([]any)
		for _, addr := range addrs {
			s, _ := addr.(string)
			if ip := globalIPv4(s); ip != "" {
				return ip
			}
		}
	}
	t.Fatalf("no global IPv4 on nic %s: %#v", nicName, instance)
	return ""
}

func nicNetworks(t *testing.T, instance map[string]any) []string {
	t.Helper()
	var out []string
	for _, raw := range asSlice(t, instance["nics"]) {
		out = append(out, asString(t, asMap(t, raw)["network"]))
	}
	return out
}

func hasNIC(t *testing.T, instance map[string]any, nicName string) bool {
	t.Helper()
	for _, raw := range asSlice(t, instance["nics"]) {
		if asString(t, asMap(t, raw)["name"]) == nicName {
			return true
		}
	}
	return false
}

func globalIPv4(raw string) string {
	host, _, ok := strings.Cut(raw, "/")
	if ok {
		raw = host
	}
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return ""
	}
	return ip.To4().String()
}

func pingAvgMS(t *testing.T, stdout string) float64 {
	t.Helper()
	summary := regexp.MustCompile(`(?m)(?:rtt|round-trip) min/avg/max(?:/mdev)? = [0-9.]+/([0-9.]+)/`)
	if match := summary.FindStringSubmatch(stdout); len(match) == 2 {
		avg, err := strconv.ParseFloat(match[1], 64)
		require.NoError(t, err, "parse ping avg from %q", stdout)
		return avg
	}
	times := regexp.MustCompile(`time[=<]([0-9.]+) ms`).FindAllStringSubmatch(stdout, -1)
	require.NotEmpty(t, times, "ping RTT not found in %q", stdout)
	var sum float64
	for _, match := range times {
		value, err := strconv.ParseFloat(match[1], 64)
		require.NoError(t, err, "parse ping time from %q", stdout)
		sum += value
	}
	return sum / float64(len(times))
}

func assertNoSandboxResidue(
	t *testing.T,
	ctx context.Context,
	backend *incus.Client,
	sandbox string,
	physical, members []string,
	forwardAddress string,
) {
	t.Helper()
	_, err := backend.GetSandbox(ctx, sandbox)
	require.Error(t, err)
	listed, err := backend.ListSandboxes(ctx)
	require.NoError(t, err)
	for _, item := range listed {
		assert.NotEqual(t, sandbox, item.Name)
	}

	project := "ac-" + sandbox
	root := backend.Scoped(ctx, api.ProjectDefaultName, "")
	_, _, err = root.GetProject(project)
	require.Error(t, err)
	projects, err := root.GetProjectNames()
	require.NoError(t, err)
	assert.NotContains(t, projects, project)

	instances, err := root.GetInstancesAllProjects(api.InstanceTypeAny)
	require.NoError(t, err)
	for _, instance := range instances {
		assert.NotEqual(t, project, instance.Project, "instance %s remained", instance.Name)
	}

	images, err := root.GetImagesAllProjects()
	require.NoError(t, err)
	for _, image := range images {
		assert.NotEqual(t, project, image.Project, "image remained in deleted project")
	}

	networks, err := root.GetNetworksAllProjects()
	require.NoError(t, err)
	for _, network := range networks {
		assert.NotEqual(t, project, network.Project, "network %s remained", network.Name)
		if network.Config["user.agentcompute.sandbox"] == sandbox {
			t.Errorf("network %s still tagged for sandbox %s", network.Name, sandbox)
		}
		for _, name := range physical {
			assert.NotEqual(t, name, network.Name, "physical network remained")
		}
		if forwardAddress != "" && network.Project == api.ProjectDefaultName {
			forwards, fwdErr := root.GetNetworkForwards(network.Name)
			if fwdErr != nil {
				continue
			}
			for _, forward := range forwards {
				assert.NotEqual(t, forwardAddress, forward.ListenAddress)
			}
		}
	}

	acls, err := root.GetNetworkACLsAllProjects()
	require.NoError(t, err)
	for _, acl := range acls {
		assert.NotEqual(t, project, acl.Project, "ACL %s remained", acl.Name)
	}

	profiles, err := root.GetProfilesAllProjects()
	require.NoError(t, err)
	for _, profile := range profiles {
		assert.NotEqual(t, project, profile.Project, "profile %s remained", profile.Name)
	}

	targets := append([]string{""}, members...)
	for _, member := range targets {
		nets, netErr := backend.Scoped(ctx, api.ProjectDefaultName, member).GetNetworks()
		require.NoError(t, netErr)
		for _, network := range nets {
			for _, name := range physical {
				assert.NotEqual(t, name, network.Name, "bridge remained on %s", member)
			}
		}
	}
}
