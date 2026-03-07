seid keys add validator --keyring-backend test --home {{.DataDir}} \
  && ADDR=$(seid keys show validator -a --keyring-backend test --home {{.DataDir}}) \
  && seid add-genesis-account "$ADDR" {{.StakeAmount}} --home {{.DataDir}} \
  && seid gentx validator {{.StakeAmount}} --chain-id {{.ChainID}} --keyring-backend test --home {{.DataDir}} \
  && mkdir -p {{.CeremonyDir}} \
  && seid tendermint show-node-id --home {{.DataDir}} > {{.CeremonyDir}}/node-id \
  && echo "$ADDR" > {{.CeremonyDir}}/address
