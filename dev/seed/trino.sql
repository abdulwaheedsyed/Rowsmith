-- Sample lake for Trino: Iceberg tables with several snapshots, and plain
-- Parquet and CSV files read through the Hive connector. Built from the
-- tpch catalog's generated data.
CREATE SCHEMA iceberg.shop;

CREATE TABLE iceberg.shop.customers
COMMENT 'People who buy things'
WITH (format = 'PARQUET')
AS SELECT custkey AS id, name, mktsegment AS segment, nationkey AS nation, acctbal AS balance,
  CAST(ROW(phone, address) AS ROW(phone varchar, address varchar)) AS contact
FROM tpch.tiny.customer;

CREATE TABLE iceberg.shop.orders
WITH (format = 'PARQUET', partitioning = ARRAY['month(placed_at)'], sorted_by = ARRAY['customer_id'])
AS SELECT orderkey AS id, custkey AS customer_id, orderstatus AS status, totalprice AS total,
  CAST(orderdate AS timestamp(6) with time zone) AS placed_at, orderpriority AS priority,
  ARRAY[clerk] AS handled_by
FROM tpch.tiny.orders WHERE orderdate < DATE '1998-01-01';

-- More snapshots: an append, an update and a delete (Iceberg v2 row-level changes).
INSERT INTO iceberg.shop.orders
SELECT orderkey, custkey, orderstatus, totalprice, CAST(orderdate AS timestamp(6) with time zone), orderpriority, ARRAY[clerk]
FROM tpch.tiny.orders WHERE orderdate >= DATE '1998-01-01';
UPDATE iceberg.shop.orders SET status = 'F' WHERE status = 'P' AND total < 20000;
DELETE FROM iceberg.shop.orders WHERE priority = '5-LOW' AND total < 1500;

CREATE TABLE iceberg.shop.order_lines
WITH (format = 'PARQUET', partitioning = ARRAY['bucket(order_id, 4)'])
AS SELECT orderkey AS order_id, linenumber AS line, partkey AS product_id, quantity, extendedprice AS price,
  discount, shipdate AS shipped_on
FROM tpch.tiny.lineitem;

CREATE VIEW iceberg.shop.revenue_by_segment AS
SELECT c.segment, count(*) AS orders, sum(o.total) AS revenue
FROM iceberg.shop.orders o JOIN iceberg.shop.customers c ON c.id = o.customer_id
GROUP BY c.segment;

-- Plain files: Parquet and CSV written as external tables in the bucket.
CREATE SCHEMA hive.raw WITH (location = 's3://lake/raw/');

CREATE TABLE hive.raw.events
WITH (format = 'PARQUET', external_location = 's3://lake/raw/events/')
AS SELECT orderkey AS order_id, 'placed' AS event, CAST(orderdate AS timestamp(3)) AS at, comment AS note
FROM tpch.tiny.orders;

CREATE TABLE hive.raw.nations
WITH (format = 'CSV', external_location = 's3://lake/raw/nations/')
AS SELECT CAST(nationkey AS varchar) AS id, CAST(name AS varchar) AS name, CAST(regionkey AS varchar) AS region FROM tpch.tiny.nation;
