# Multi-replica cluster quickstart

For a one-command `docker compose up` cluster (2 loomcycle replicas + Postgres + nginx LB) with a verify script that exercises cluster-mode invariants, see:

**[`examples/cluster/README.md`](../examples/cluster/README.md)**

For the full operator runbook — deployment shape, rolling upgrade, crashed-replica recovery, pool sizing, sharp edges — see [`MULTI-REPLICA.md`](MULTI-REPLICA.md).
