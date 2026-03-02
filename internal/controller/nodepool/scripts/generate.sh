#!/bin/sh
# shellcheck shell=sh
# Sei Multi-Node Genesis Generator (BusyBox/POSIX & idempotent)
# Now powered by seictl for config and genesis manipulation

set -eu

###############################################################################
# Config (env overrides OK)
###############################################################################
NUM_NODES=${NUM_NODES:-4}
CHAIN_ID=${CHAIN_ID:-sei}
NODES_DIR=${NODES_DIR:-"nodes"}
SEID_BIN=${SEID_BIN:-"seid"}
SEICTL_BIN=${SEICTL_BIN:-"seictl"}
PRICE_FEEDER_BIN=${PRICE_FEEDER_BIN:-"price-feeder"} # unused but preserved
PASSWORD=${PASSWORD:-"12345678"}

# Optional: allow caller to override release schedule dates
START_DATE=${START_DATE:-""}
END_DATE=${END_DATE:-""}

###############################################################################
# Logging helpers (consistent output)
###############################################################################
log()  { printf '%s\n' "$*"; }
sep()  { printf '%s\n' "=========================================="; }
step() { printf '\n%s\n%s\n%s\n' "==========================================" "$*" "=========================================="; }

###############################################################################
# Utilities & checks
###############################################################################
require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'Error: required command not found: %s\n' "$1" >&2
    exit 1
  fi
}

# BusyBox/posix-friendly mktemp fallback

# POSIX/BusyBox-friendly integer loop: i=0..NUM_NODES-1
loop_nodes() {
  i=0
  while [ "$i" -lt "$NUM_NODES" ]; do
    printf '%s\n' "$i"
    i=$((i + 1))
  done
}

# Date helpers with portability (GNU date -d, BSD date -v, or fallback)
today_ymd() {
  date +"%Y-%m-%d" 2>/dev/null || date -I 2>/dev/null || printf '1970-01-01'
}
plus_days_ymd() {
  _days="$1"
  # Try GNU date
  if date -d "+${_days} days" +"%Y-%m-%d" >/dev/null 2>&1; then
    date -d "+${_days} days" +"%Y-%m-%d"
    return
  fi
  # Try BSD date
  if date -v+"${_days}"d +"%Y-%m-%d" >/dev/null 2>&1; then
    date -v+"${_days}"d +"%Y-%m-%d"
    return
  fi
  # Fallback: same day (no arithmetic available)
  today_ymd
}

###############################################################################
# Pre-flight
###############################################################################
require_cmd "$SEID_BIN"
require_cmd "$SEICTL_BIN"
require_cmd wc
require_cmd cp

require_cmd rm
require_cmd mkdir
require_cmd printf
require_cmd awk

# Ensure base dir exists
mkdir -p "$NODES_DIR"

MARKER_FILE="$NODES_DIR/.genesis_ready"
if [ -f "$MARKER_FILE" ]; then
  sep
  log "Sei Multi-Node Genesis Generator"
  sep
  log "Detected previous successful run at: $MARKER_FILE"
  log "Skipping regeneration. Output directory: $NODES_DIR"
  exit 0
fi

sep
log "Sei Multi-Node Genesis Generator (powered by seictl)"
sep
log "Number of nodes: $NUM_NODES"
log "Chain ID: $CHAIN_ID"
log "Output directory: $NODES_DIR"
log "Seid binary: $SEID_BIN"
log "Seictl binary: $SEICTL_BIN"
sep

step "STEP 0: Preparing Directories"

# Clean only if not already successful (marker not present)
# Remove pre-existing partial state to ensure clean run
if [ -d "$NODES_DIR" ]; then
  rm -rf "$NODES_DIR"
fi
mkdir -p "$NODES_DIR"

# Files we will (re)build
PERSISTENT_PEERS_FILE="$NODES_DIR/persistent_peers.txt"
GENESIS_ACCOUNTS_FILE="$NODES_DIR/genesis_accounts.txt"
: >"$PERSISTENT_PEERS_FILE"
: >"$GENESIS_ACCOUNTS_FILE"

step "STEP 1: Initializing All Nodes"

for NODE_ID in $(loop_nodes); do
  printf '\n%s\n' "--- Initializing Node $NODE_ID ---"

  NODE_HOME="$NODES_DIR/node$NODE_ID"
  mkdir -p "$NODE_HOME"

  MONIKER="node$NODE_ID"
  log "Initializing node: $MONIKER"
  # init
  if ! "$SEID_BIN" init "$MONIKER" --chain-id "$CHAIN_ID" --home "$NODE_HOME" >/dev/null 2>&1; then
    printf 'Error: failed to init node %s\n' "$NODE_ID" >&2
    exit 1
  fi

  # peer id
  NODE_PEER_ID=$("$SEID_BIN" tendermint show-node-id --home "$NODE_HOME")
  PEER_PORT=26656
  printf '%s\n' "${NODE_PEER_ID}@${MONIKER}:${PEER_PORT}" >>"$PERSISTENT_PEERS_FILE"

  # key/account
  ACCOUNT_NAME="node_admin"
  log "Creating account: $ACCOUNT_NAME"
  # shellcheck disable=SC3037 # printf is POSIX; echo -n is not used.
  { printf '%s\n' "$PASSWORD"; printf '%s\n' "$PASSWORD"; } | \
    "$SEID_BIN" keys add "$ACCOUNT_NAME" \
      --keyring-backend file --home "$NODE_HOME" \
      --keyring-backend file --output json > "${NODE_HOME}/config/admin_key.json"
  
  # Note to all mere mortals: path to admin key has to be that. It is hard-coded in 
  # sei-chain and there is no documentation other than the code.
  # See: https://github.com/sei-protocol/sei-chain/blob/07441d7bfcd7f9fc69119cf3002be7d6912b3a87/loadtest/sign.go#L91
  
  NODE_ADDRESS=$(
    printf '%s\n' "$PASSWORD" | \
      "$SEID_BIN" keys show "$ACCOUNT_NAME" -a \
        --keyring-backend file --home "$NODE_HOME" \
        --keyring-backend file
  )
  printf '%s\n' "$NODE_ADDRESS" >>"$GENESIS_ACCOUNTS_FILE"
 
  log "Adding genesis account funds"
  "$SEID_BIN" add-genesis-account "$NODE_ADDRESS" \
    10000000usei,10000000uusdc,10000000uatom \
    --keyring-backend file --home "$NODE_HOME" \
    --keyring-backend file >/dev/null

  log "Creating gentx $NODE_HOME"
  printf '%s\n' "$PASSWORD" | \
    "$SEID_BIN" gentx "$ACCOUNT_NAME" 10000000usei \
      --chain-id "$CHAIN_ID" \
      --keyring-backend file --home "$NODE_HOME" \
      --keyring-backend file >/dev/null

  log "Node $NODE_ID initialized: $NODE_ADDRESS"
done

step "STEP 2: Building Genesis on Node 0"

NODE0_HOME="$NODES_DIR/node0"

log "Creating admin account..."
{ printf '%s\n' "$PASSWORD"; printf '%s\n' "$PASSWORD"; } | \
  "$SEID_BIN" keys add "admin" \
    --keyring-backend file --home "$NODE0_HOME" \
    --keyring-backend file >/dev/null 2>&1

ADMIN_ADDRESS=$(
  printf '%s\n' "$PASSWORD" | \
    "$SEID_BIN" keys show "admin" -a \
      --keyring-backend file --home "$NODE0_HOME" \
      --keyring-backend file
)

# seictl-based genesis patching helper
patch_genesis() {
  patch_content=$1
  printf '%s' "$patch_content" | "$SEICTL_BIN" --home "$NODE0_HOME" genesis patch -i
}

log "Configuring genesis parameters..."

# Token release schedule (portable dates)
if [ -z "$START_DATE" ]; then
  START_DATE=$(today_ymd)
fi
if [ -z "$END_DATE" ]; then
  END_DATE=$(plus_days_ymd 3)
fi

# Build comprehensive genesis patch - combining related parameters
# We can apply this as a single patch for better performance
GENESIS_PATCH=$(cat <<EOF
{
  "app_state": {
    "crisis": {
      "constant_fee": {
        "denom": "usei"
      }
    },
    "mint": {
      "params": {
        "mint_denom": "usei",
        "token_release_schedule": [
          {
            "start_date": "$START_DATE",
            "end_date": "$END_DATE",
            "token_release_amount": "999999999999"
          }
        ]
      }
    },
    "staking": {
      "params": {
        "bond_denom": "usei",
        "max_validators": "50",
        "unbonding_time": "10s"
      }
    },
    "oracle": {
      "params": {
        "vote_period": "2"
      }
    },
    "slashing": {
      "params": {
        "signed_blocks_window": "10000",
        "min_signed_per_window": "0.050000000000000000"
      }
    },
    "auth": {
      "accounts": []
    },
    "bank": {
      "balances": [],
      "denom_metadata": [
        {
          "denom_units": [
            {
              "denom": "UATOM",
              "exponent": 6,
              "aliases": ["UATOM"]
            }
          ],
          "base": "uatom",
          "display": "uatom",
          "name": "UATOM",
          "symbol": "UATOM"
        }
      ]
    },
    "genutil": {
      "gen_txs": []
    },
    "evm": {
      "params": {
        "sei_sstore_set_gas_eip2200": 20000
      }
    },
    "gov": {
      "deposit_params": {
        "min_deposit": [
          {
            "denom": "usei"
          }
        ],
        "min_expedited_deposit": [
          {
            "denom": "usei"
          }
        ],
        "max_deposit_period": "100s"
      },
      "voting_params": {
        "voting_period": "30s",
        "expedited_voting_period": "15s"
      },
      "tally_params": {
        "quorum": "0.5",
        "threshold": "0.5",
        "expedited_quorum": "0.9",
        "expedited_threshold": "0.9"
      }
    }
  },
  "consensus_params": {
    "block": {
      "max_gas": "35000000"
    }
  }
}
EOF
)

log "Applying genesis configuration patch..."
patch_genesis "$GENESIS_PATCH"

# Add genesis accounts
log "Adding genesis accounts..."
# Use while+read with IFS and -r for safety
if [ -f "$GENESIS_ACCOUNTS_FILE" ]; then
  # shellcheck disable=SC2034 # keep line count displayed later
  GEN_COUNT=$(wc -l <"$GENESIS_ACCOUNTS_FILE" | awk '{print $1}')
  while IFS= read -r account || [ -n "$account" ]; do
    [ -z "$account" ] && continue
    log "  Adding: $account"
    "$SEID_BIN" add-genesis-account "$account" \
      1000000000000000000000usei,1000000000000000000000uusdc,1000000000000000000000uatom \
      --keyring-backend file --home "$NODE0_HOME" >/dev/null
  done <"$GENESIS_ACCOUNTS_FILE"
else
  printf 'Error: missing %s\n' "$GENESIS_ACCOUNTS_FILE" >&2
  exit 1
fi

log "  Adding admin: $ADMIN_ADDRESS"
"$SEID_BIN" add-genesis-account "$ADMIN_ADDRESS" \
  1000000000000000000000usei,1000000000000000000000uusdc,1000000000000000000000uatom \
  --keyring-backend file --home "$NODE0_HOME" >/dev/null

step "STEP 3: Collecting Genesis Transactions"

mkdir -p "$NODES_DIR/gentx_temp"

for NODE_ID in $(loop_nodes); do
  NODE_HOME="$NODES_DIR/node$NODE_ID"
  if [ -d "$NODE_HOME/config/gentx" ]; then
    log "Collecting gentx from node $NODE_ID..."
    # shellcheck disable=SC2045 # iterating over files from globbed dir; path contains no spaces by construction
    for f in $(ls "$NODE_HOME/config/gentx" 2>/dev/null || true); do
      if [ -f "$NODE_HOME/config/gentx/$f" ]; then
        cp "$NODE_HOME/config/gentx/$f" "$NODES_DIR/gentx_temp/" || true
      fi
    done
  fi
done

# Ensure node0 gentx dir exists
mkdir -p "$NODE0_HOME/config/gentx"
log "Copying all gentx files to node 0..."
# Copy only if any exist
for f in "$NODES_DIR"/gentx_temp/*; do
  [ -f "$f" ] || continue
  cp "$f" "$NODE0_HOME/config/gentx/"
done

log "Finalizing genesis..."
"$SEID_BIN" collect-gentxs --home "$NODE0_HOME" >/dev/null 2>&1 || {
  printf 'Error: collect-gentxs failed\n' >&2
  exit 1
}

step "STEP 4: Distributing Genesis to All Nodes"

cp "$NODE0_HOME/config/genesis.json" "$NODES_DIR/genesis.json"
log "Final genesis saved to: $NODES_DIR/genesis.json"

for NODE_ID in $(loop_nodes); do
  NODE_HOME="$NODES_DIR/node$NODE_ID"
  log "Copying genesis to node $NODE_ID..."
  mkdir -p "$NODE_HOME/config"
  cp "$NODES_DIR/genesis.json" "$NODE_HOME/config/genesis.json"
done

# Success marker to enforce idempotency on next runs
date +'OK %Y-%m-%dT%H:%M:%SZ' >"$MARKER_FILE"

step "Genesis Generation Complete!"

log "Output directory: $NODES_DIR"
printf '\n%s\n' "Directory structure:"
printf '  %s/\n' "$NODES_DIR"
printf '  |-- genesis.json\n'
printf '  |-- genesis_accounts.txt\n'
printf '  |-- persistent_peers.txt\n'
printf '  |-- node0/\n'
printf '  |   |-- config/\n'
printf '  |   |   |-- genesis.json\n'
printf '  |   |   |-- app.toml\n'
printf '  |   |   \`-- config.toml\n'
printf '  |   |-- keyring-file/\n'
printf '  |   \`-- data/\n'
printf '  |-- node1/\n'
printf '  |-- node2/\n'
printf '  \`-- node3/\n'

VALS="$NUM_NODES"
ACCTS=$(wc -l <"$GENESIS_ACCOUNTS_FILE" | awk '{print $1}')
printf '\nNumber of validators: %s\n' "$VALS"
printf 'Number of accounts: %s\n\n' "$ACCTS"

printf '%s\n' "To start nodes:"
printf '  # Node 0 (ports 26656, 26657, 1317, 9090)\n'
printf '  %s start --keyring-backend file --home %s/node0\n\n' "$SEID_BIN" "$NODES_DIR"
printf '  # Node 1 (ports 26657, 26658, 1318, 9091)\n'
printf '  %s start --keyring-backend file --home %s/node1\n\n' "$SEID_BIN" "$NODES_DIR"
printf 'All accounts use keyring-backend file with password: %s\n' "$PASSWORD"
sep