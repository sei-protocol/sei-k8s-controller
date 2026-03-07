#!/bin/sh
set -eu

# Copy the node's genesis directory into the data volume.
cp -a "${GENESIS_NODES_DIR}/node${NODE_INDEX}/." "${DATA_DIR}/"

# Rewrite persistent-peers from local node references to Kubernetes DNS.
if [ -f "${GENESIS_NODES_DIR}/persistent_peers.txt" ]; then
  PEERS=$(sed "s|@node\([0-9]*\):|@${POOL_NAME}-\1-0.${POOL_NAME}-\1.${NAMESPACE}.svc.cluster.local:|g" \
    "${GENESIS_NODES_DIR}/persistent_peers.txt" | tr '\n' ',' | sed 's/,$//')
  sed -i "s|^persistent-peers *=.*|persistent-peers = \"${PEERS}\"|" "${DATA_DIR}/config/config.toml"
fi

sed -i "s|^mode *=.*|mode = \"validator\"|" "${DATA_DIR}/config/config.toml"
sed -i "s|^queue-type *=.*|queue-type = \"fifo\"|" "${DATA_DIR}/config/config.toml"
sed -i "s|^slow *=.*|slow = true|" "${DATA_DIR}/config/app.toml"
