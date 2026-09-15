#!/bin/sh
# RFC DB-2 — measure the SHAPE of the graph consolidation actually built.
#
# A traversal arm needs a CHAIN: A->B and B->C. What the consolidation path
# produces is a STAR -- a fact node with one `about` edge to its own subject --
# unless the extractor also emits `also_about` for the second entity the fact
# names. So the number that decides whether P5 has anything to walk is the
# fraction of facts carrying TWO subject edges, measured against a corpus where
# the ground truth is 100% by construction.
#
#   ./graph_shape.sh <sqlmem-db> [schema]
set -e
DB=${1:-loomcycle_db2_sqlmem}
# Pick the busiest scope schema by ROW count. Choosing by table count picks an
# empty scope, because every scope has the same tables.
if [ -z "$2" ]; then
  S=""; best=-1
  for cand in $(psql -d "$DB" -tAc "select nspname from pg_namespace where nspname like 'sqlmem_s_%'"); do
    n=$(psql -d "$DB" -tAc "select count(*) from $cand.chunks" 2>/dev/null || echo 0)
    [ "$n" -gt "$best" ] && { best=$n; S=$cand; }
  done
else
  S=$2
fi
echo "schema: $S"
psql -d "$DB" -tAc "select 'fact chunks: ' || count(*) from $S.chunks where type='fact';"
psql -d "$DB" -tAc "select 'entity nodes: ' || count(*) from $S.chunks where type is not null and type <> 'fact';"
psql -d "$DB" -tAc "select 'edges: ' || count(*) || ' (kinds: ' || string_agg(distinct kind, ', ') || ')' from $S.chunk_edges;"
echo "--- subject edges per fact (2+ is what a chain needs) ---"
psql -d "$DB" -c "
select n_edges as subject_edges, count(*) as facts,
       round(100.0*count(*)/sum(count(*)) over (), 1) as pct
from (select c.id, count(e.from_id) as n_edges
      from $S.chunks c left join $S.chunk_edges e on e.from_id = c.id
      where c.type='fact' group by c.id) t
group by 1 order by 1;"
