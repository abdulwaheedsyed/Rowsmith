-- Delta Lake tables for the dev stack's Trino (see dev/compose.yaml), from
-- the tpch catalog. Several commits, so the table has a history.
CREATE SCHEMA delta.sales;

CREATE TABLE delta.sales.payments
WITH (partitioned_by = ARRAY['region'], checkpoint_interval = 5, change_data_feed_enabled = true)
AS SELECT o.orderkey AS id, o.custkey AS customer_id,
  CASE o.orderkey % 3 WHEN 0 THEN 'card' WHEN 1 THEN 'transfer' ELSE 'wallet' END AS method,
  CAST(o.totalprice AS decimal(12,2)) AS amount,
  CAST(o.orderdate AS timestamp(3) with time zone) AS paid_at,
  MAP(ARRAY['channel', 'clerk'], ARRAY[CASE WHEN o.orderkey % 5 = 0 THEN 'phone' ELSE 'web' END, o.clerk]) AS attributes,
  lower(r.name) AS region
FROM tpch.tiny.orders o
JOIN tpch.tiny.customer c ON c.custkey = o.custkey
JOIN tpch.tiny.nation n ON n.nationkey = c.nationkey
JOIN tpch.tiny.region r ON r.regionkey = n.regionkey
WHERE o.orderdate < DATE '1995-01-01';

COMMENT ON TABLE delta.sales.payments IS 'Payments by region, from tpch orders';

-- More commits: an append, an update and a delete.
INSERT INTO delta.sales.payments
SELECT o.orderkey, o.custkey, 'card', CAST(o.totalprice AS decimal(12,2)), CAST(o.orderdate AS timestamp(3) with time zone),
  MAP(ARRAY['channel'], ARRAY['web']), lower(r.name)
FROM tpch.tiny.orders o
JOIN tpch.tiny.customer c ON c.custkey = o.custkey
JOIN tpch.tiny.nation n ON n.nationkey = c.nationkey
JOIN tpch.tiny.region r ON r.regionkey = n.regionkey
WHERE o.orderdate >= DATE '1995-01-01' AND o.orderdate < DATE '1995-07-01';
UPDATE delta.sales.payments SET method = 'card' WHERE method = 'wallet' AND amount < 5000;
DELETE FROM delta.sales.payments WHERE amount < 1500;
