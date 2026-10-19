-- Sample "shop" schema for Snowflake: run in a database of your own (the
-- integration tests create one, ROWSMITH_IT_<random>, and drop it after).
-- Snowflake has no local server, so this is the only seed that runs in the
-- cloud: keep it small to spend as little warehouse time as possible.
CREATE SCHEMA shop COMMENT = 'Sample shop';
USE SCHEMA shop;

CREATE TABLE categories (
  id NUMBER(9,0) NOT NULL PRIMARY KEY,
  name VARCHAR(80) NOT NULL UNIQUE,
  parent_id NUMBER(9,0) REFERENCES categories (id)
) COMMENT = 'Product taxonomy';

CREATE TABLE customers (
  id NUMBER(38,0) IDENTITY(1,1) PRIMARY KEY,
  email VARCHAR(190) NOT NULL,
  full_name VARCHAR(120) NOT NULL COMMENT 'As the customer typed it',
  tier VARCHAR(12) NOT NULL DEFAULT 'free',
  is_active BOOLEAN NOT NULL DEFAULT TRUE,
  preferences VARIANT,
  tags ARRAY,
  address OBJECT,
  created_at TIMESTAMP_NTZ NOT NULL DEFAULT CURRENT_TIMESTAMP()
) COMMENT = 'People who buy things';

CREATE TABLE products (
  id NUMBER(9,0) NOT NULL PRIMARY KEY,
  sku VARCHAR(32) NOT NULL UNIQUE,
  name VARCHAR(160) NOT NULL,
  category_id NUMBER(9,0) NOT NULL REFERENCES categories (id),
  price NUMBER(10,2) NOT NULL,
  weight_kg FLOAT,
  thumbnail BINARY,
  launched_on DATE
);

CREATE TABLE orders (
  id NUMBER(38,0) NOT NULL PRIMARY KEY,
  customer_id NUMBER(38,0) NOT NULL REFERENCES customers (id),
  status VARCHAR(12) NOT NULL DEFAULT 'pending',
  total NUMBER(12,2) NOT NULL DEFAULT 0,
  placed_at TIMESTAMP_TZ NOT NULL,
  ship_by TIME,
  notes STRING
) CLUSTER BY (placed_at);

CREATE TABLE stores (
  id NUMBER(9,0) NOT NULL PRIMARY KEY,
  name VARCHAR(80) NOT NULL,
  location GEOGRAPHY,
  footprint GEOMETRY
);

CREATE TABLE audit_events (
  happened_at TIMESTAMP_LTZ NOT NULL,
  actor VARCHAR(80) NOT NULL,
  action VARCHAR(40) NOT NULL,
  payload VARIANT
) COMMENT = 'Deliberately has no primary key';

CREATE SEQUENCE invoice_numbers START = 1000 INCREMENT = 1;

INSERT INTO categories (id, name, parent_id) VALUES
 (1, 'Electronics', NULL), (2, 'Home', NULL), (3, 'Outdoors', NULL), (4, 'Books', NULL),
 (5, 'Laptops', 1), (6, 'Phones', 1), (7, 'Audio', 1), (8, 'Kitchen', 2), (9, 'Furniture', 2), (10, 'Camping', 3);

INSERT INTO customers (email, full_name, tier, is_active, preferences, tags, address, created_at)
SELECT 'user' || (seq4() + 1) || '@example.com',
       ARRAY_CONSTRUCT('Ava', 'Liam', 'Mia', 'Noah', 'Zoe', 'Omar')[(seq4() % 6)]::STRING || ' ' || ARRAY_CONSTRUCT('Smith', 'Garcia', 'Chen', 'Okafor')[(seq4() % 4)]::STRING,
       ARRAY_CONSTRUCT('free', 'pro', 'enterprise')[(seq4() % 3)]::STRING,
       seq4() % 17 <> 0,
       OBJECT_CONSTRUCT('newsletter', seq4() % 2 = 0, 'theme', IFF(seq4() % 2 = 0, 'dark', 'light')),
       ARRAY_CONSTRUCT('beta', 'cohort-' || (seq4() % 5)),
       OBJECT_CONSTRUCT('city', ARRAY_CONSTRUCT('Amsterdam', 'Utrecht', 'Riyadh')[(seq4() % 3)]::STRING, 'zip', LPAD(seq4() % 9999, 4, '0')),
       DATEADD(minute, seq4() * 37, '2025-01-01'::TIMESTAMP_NTZ)
FROM TABLE(GENERATOR(ROWCOUNT => 500));

INSERT INTO products (id, sku, name, category_id, price, weight_kg, launched_on)
SELECT seq4() + 1, 'SKU-' || LPAD(seq4() + 1, 5, '0'),
       ARRAY_CONSTRUCT('Aurora', 'Nimbus', 'Summit', 'Harbor')[(seq4() % 4)]::STRING || ' ' || ARRAY_CONSTRUCT('Pro', 'Mini', 'Max')[(seq4() % 3)]::STRING,
       5 + seq4() % 6, ROUND(9.99 + (seq4() * 7.31) % 990, 2), ROUND(0.1 + (seq4() % 50) / 7.0, 2),
       DATEADD(day, seq4(), '2024-01-01'::DATE)
FROM TABLE(GENERATOR(ROWCOUNT => 100));

UPDATE products SET thumbnail = TO_BINARY('89504E470D0A1A0A0000000D4948445200000001000000010806000000'
  || '1F15C4890000000D49444154789C63F8CFC0F01F0005000201F5E2E2A60000000049454E44AE426082', 'HEX') WHERE id <= 5;

INSERT INTO orders (id, customer_id, status, total, placed_at, ship_by, notes)
SELECT seq4() + 1, 1 + (seq4() * 7) % 500,
       ARRAY_CONSTRUCT('pending', 'paid', 'shipped', 'delivered', 'refunded')[(seq4() % 5)]::STRING,
       ROUND((seq4() * 13.37) % 900 + 5, 2),
       DATEADD(minute, seq4() * 13, '2025-06-01 00:00:00 +00:00'::TIMESTAMP_TZ),
       TIME_FROM_PARTS(9 + seq4() % 8, 0, 0),
       IFF(seq4() % 9 = 0, 'Leave at the door', NULL)
FROM TABLE(GENERATOR(ROWCOUNT => 2000));

INSERT INTO stores (id, name, location, footprint)
SELECT column1, column2, TO_GEOGRAPHY(column3), TO_GEOMETRY(column4) FROM VALUES
 (1, 'Canal Street', 'POINT(4.8952 52.3702)', 'POLYGON((0 0, 10 0, 10 10, 0 10, 0 0))'),
 (2, 'Harbour Front', 'POINT(4.4777 51.9244)', 'POLYGON((0 0, 5 0, 5 5, 0 5, 0 0))'),
 (3, 'Old Town', 'POINT(5.1214 52.0907)', NULL);

INSERT INTO audit_events (happened_at, actor, action, payload)
SELECT DATEADD(hour, seq4(), '2026-01-01'::TIMESTAMP_LTZ), ARRAY_CONSTRUCT('system', 'admin', 'api')[(seq4() % 3)]::STRING,
       ARRAY_CONSTRUCT('login', 'export', 'refund')[(seq4() % 3)]::STRING, OBJECT_CONSTRUCT('n', seq4())
FROM TABLE(GENERATOR(ROWCOUNT => 50));

CREATE VIEW customer_revenue COMMENT = 'Revenue per customer' AS
SELECT c.id, c.full_name, c.tier, COUNT(o.id) AS orders, COALESCE(SUM(o.total), 0) AS revenue
FROM customers c LEFT JOIN orders o ON o.customer_id = c.id AND o.status IN ('paid', 'shipped', 'delivered')
GROUP BY c.id, c.full_name, c.tier;

CREATE FUNCTION order_count(cust NUMBER) RETURNS NUMBER AS
$$
  SELECT COUNT(*) FROM orders WHERE customer_id = cust
$$;

CREATE PROCEDURE restock(product NUMBER, qty NUMBER)
RETURNS STRING
LANGUAGE SQL
AS
$$
BEGIN
  IF (qty <= 0) THEN
    RETURN 'quantity must be positive';
  END IF;
  UPDATE products SET weight_kg = weight_kg WHERE id = :product;
  RETURN 'restocked ' || qty;
END;
$$;
