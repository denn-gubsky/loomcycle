-- v0.12.0 multi-replica HA foundation.
--
-- One row per loomcycle replica running against this Postgres instance.
-- A replica self-registers on boot (UPSERT every 30s via heartbeat),
-- self-deletes on graceful shutdown. /healthz aggregates this table
-- into the cluster view; later phases use it to detect dead-owner
-- replicas for cancel/status fallback.
--
-- Only populated when LOOMCYCLE_REPLICA_ID is set — single-replica
-- deployments leave the table empty.

CREATE TABLE IF NOT EXISTS replicas (
    id                TEXT        PRIMARY KEY,
    hostname          TEXT        NOT NULL DEFAULT '',
    started_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    version           TEXT        NOT NULL DEFAULT ''
);
