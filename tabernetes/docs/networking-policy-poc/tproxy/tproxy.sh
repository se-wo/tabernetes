#!/bin/sh
# Proof of concept for docs/networking-policy.md, run by ../run.sh tproxy in
# a new user, mount and network namespace, like the agent of a rootless Docker
# node. A "pod" in a second network namespace (a flannel pod on cni0) connects
# to 10.244.0.7, a pod IP of a tab node. TPROXY hands the connection to a
# listener of the "agent" with the source pod IP and the original destination
# unchanged.
set -e
mount -t tmpfs none /run
ip link set lo up
ip link add veth0 type veth peer name veth1
ip addr add 10.244.1.1/24 dev veth0
ip link set veth0 up
mkdir -p /run/netns
ip netns add pod
ip link set veth1 netns pod
ip netns exec pod sh -c 'ip link set lo up; ip addr add 10.244.1.5/24 dev veth1; ip link set veth1 up; ip route add default via 10.244.1.1'
nft add table inet tp
nft 'add chain inet tp pre { type filter hook prerouting priority mangle; }'
nft 'add rule inet tp pre ip daddr 10.244.0.0/24 meta l4proto tcp tproxy ip to :15001 meta mark set 1'
ip rule add fwmark 1 lookup 100
ip route add local 0.0.0.0/0 dev lo table 100
python3 -I "$1" &
sleep 0.5
ip netns exec pod python3 -I -c 'import socket; s = socket.create_connection(("10.244.0.7", 80), 2); print("pod connected from", s.getsockname())'
wait
