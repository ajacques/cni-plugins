set -ex
set -e

MASTER_IFACE="eth0"

# https://github.com/k3s-io/k3s/blob/02a5bee62f5d7a0811a6a9401fc9ff5b1399c0bc/scripts/package-cli#L15-L18
# Some distros, like k3s, symlink CNIs to the same binary. Trying to copy files like this will
# mean we corrupt the binaries
for filename in /cni/*; do
  [ -e /host/cni_bin/$(basename $filename) ] && rm /host/cni_bin/$(basename $filename)
  cp $filename /host/cni_bin/$(basename $filename)
done

sed "s/MASTER_IFACE/$MASTER_IFACE/g" bridge-cni.tmpl > /host/cni_net/${PRIORITY:-10}-bridge.conflist

rm -f /run/cni/dhcp.sock

exec /cni/dhcp daemon
