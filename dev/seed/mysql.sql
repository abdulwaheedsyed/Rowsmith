-- Sample "shop" database for MySQL 8 / MariaDB 11 (portable syntax).
CREATE TABLE categories (
  id INT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(80) NOT NULL UNIQUE,
  parent_id INT UNSIGNED NULL,
  CONSTRAINT fk_cat_parent FOREIGN KEY (parent_id) REFERENCES categories(id) ON DELETE SET NULL
) COMMENT='Product taxonomy';

CREATE TABLE customers (
  id INT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  email VARCHAR(190) NOT NULL UNIQUE,
  full_name VARCHAR(120) NOT NULL,
  tier ENUM('free','pro','enterprise') NOT NULL DEFAULT 'free',
  country CHAR(2) NOT NULL DEFAULT 'US',
  is_active TINYINT(1) NOT NULL DEFAULT 1,
  preferences JSON NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP NULL DEFAULT NULL ON UPDATE CURRENT_TIMESTAMP,
  KEY idx_customers_country (country, tier)
) COMMENT='People who buy things';

CREATE TABLE products (
  id INT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  sku VARCHAR(32) NOT NULL,
  name VARCHAR(160) NOT NULL,
  category_id INT UNSIGNED NOT NULL,
  price DECIMAL(10,2) NOT NULL,
  cost DECIMAL(10,2) NOT NULL,
  margin DECIMAL(10,2) AS (price - cost) VIRTUAL,
  stock INT NOT NULL DEFAULT 0,
  weight_kg FLOAT NULL,
  description TEXT NULL,
  thumbnail MEDIUMBLOB NULL,
  UNIQUE KEY uq_products_sku (sku),
  KEY idx_products_category (category_id),
  CONSTRAINT fk_products_category FOREIGN KEY (category_id) REFERENCES categories(id)
);

CREATE TABLE stores (
  id INT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(80) NOT NULL,
  city VARCHAR(80) NOT NULL,
  location POINT NOT NULL,
  opened_on DATE NOT NULL,
  SPATIAL KEY sp_stores_location (location)
);

CREATE TABLE orders (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  customer_id INT UNSIGNED NOT NULL,
  store_id INT UNSIGNED NULL,
  status ENUM('pending','paid','shipped','delivered','refunded','cancelled') NOT NULL DEFAULT 'pending',
  total DECIMAL(12,2) NOT NULL DEFAULT 0,
  placed_at DATETIME(3) NOT NULL,
  notes VARCHAR(500) NULL,
  KEY idx_orders_customer (customer_id, placed_at),
  KEY idx_orders_status (status),
  CONSTRAINT fk_orders_customer FOREIGN KEY (customer_id) REFERENCES customers(id) ON DELETE CASCADE,
  CONSTRAINT fk_orders_store FOREIGN KEY (store_id) REFERENCES stores(id)
);

CREATE TABLE order_items (
  order_id BIGINT UNSIGNED NOT NULL,
  line_no SMALLINT UNSIGNED NOT NULL,
  product_id INT UNSIGNED NOT NULL,
  quantity INT NOT NULL,
  unit_price DECIMAL(10,2) NOT NULL,
  PRIMARY KEY (order_id, line_no),
  CONSTRAINT fk_items_order FOREIGN KEY (order_id) REFERENCES orders(id) ON DELETE CASCADE,
  CONSTRAINT fk_items_product FOREIGN KEY (product_id) REFERENCES products(id),
  CONSTRAINT chk_quantity CHECK (quantity > 0)
);

CREATE TABLE audit_events (
  happened_at DATETIME NOT NULL,
  actor VARCHAR(80) NOT NULL,
  action VARCHAR(40) NOT NULL,
  payload JSON NULL
) COMMENT='Deliberately has no primary key';

-- A digits table makes bulk generation portable across MySQL and MariaDB.
CREATE TABLE _digits (d INT PRIMARY KEY);
INSERT INTO _digits VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9);
CREATE TABLE _seq (n INT PRIMARY KEY);
INSERT INTO _seq SELECT a.d + b.d*10 + c.d*100 + e.d*1000 + f.d*10000 + 1
  FROM _digits a, _digits b, _digits c, _digits e, _digits f;

INSERT INTO categories (name, parent_id) VALUES
 ('Electronics', NULL), ('Home', NULL), ('Outdoors', NULL), ('Books', NULL),
 ('Laptops', 1), ('Phones', 1), ('Audio', 1), ('Kitchen', 2), ('Furniture', 2), ('Camping', 3), ('Cycling', 3), ('Fiction', 4);

INSERT INTO customers (email, full_name, tier, country, is_active, preferences, created_at)
SELECT CONCAT('user', n, '@example.com'),
       CONCAT(ELT(1 + n % 12, 'Ava','Liam','Mia','Noah','Zoe','Omar','Lena','Kai','Ines','Yuki','Ravi','Sofia'), ' ',
              ELT(1 + (n DIV 12) % 10, 'Smith','Garcia','Chen','Okafor','Silva','Novak','Haddad','Kim','Rossi','Berg')),
       ELT(1 + n % 3, 'free','pro','enterprise'),
       ELT(1 + n % 8, 'US','GB','DE','BR','JP','IN','NG','AU'),
       n % 17 <> 0,
       JSON_OBJECT('newsletter', n % 2 = 0, 'theme', ELT(1 + n % 2, 'dark', 'light'), 'tags', JSON_ARRAY('beta', n % 5)),
       TIMESTAMP('2023-01-01') + INTERVAL n * 37 MINUTE
FROM _seq WHERE n <= 5000;

INSERT INTO products (sku, name, category_id, price, cost, stock, weight_kg, description)
SELECT CONCAT('SKU-', LPAD(n, 5, '0')),
       CONCAT(ELT(1 + n % 8, 'Aurora','Nimbus','Summit','Harbor','Cobalt','Ember','Tidal','Quartz'), ' ',
              ELT(1 + n % 12, 'Pro','Mini','Max','Air','Lite','One','X','Plus','Go','Studio','Edge','Core')),
       5 + n % 8,
       ROUND(9.99 + (n * 7.31) % 990, 2),
       ROUND((9.99 + (n * 7.31) % 990) * 0.62, 2),
       (n * 13) % 400,
       ROUND(0.1 + (n % 50) / 7.0, 2),
       CONCAT('Well-made item number ', n, '. Ships in recyclable packaging.')
FROM _seq WHERE n <= 400;

-- A 1x1 PNG so the grid can preview images stored in BLOBs.
UPDATE products SET thumbnail = UNHEX('89504E470D0A1A0A0000000D4948445200000001000000010806000000'
  '1F15C4890000000D49444154789C63F8CFC0F01F0005000201F5E2E2A60000000049454E44AE426082') WHERE id <= 40;

INSERT INTO stores (name, city, location, opened_on) VALUES
 ('Canal Street', 'Amsterdam', ST_GeomFromText('POINT(4.8952 52.3702)'), '2019-04-01'),
 ('Harbour Front', 'Rotterdam', ST_GeomFromText('POINT(4.4777 51.9244)'), '2020-09-15'),
 ('Old Town', 'Utrecht', ST_GeomFromText('POINT(5.1214 52.0907)'), '2021-02-20'),
 ('Beach Road', 'The Hague', ST_GeomFromText('POINT(4.3007 52.0705)'), '2022-06-11'),
 ('Tech Park', 'Eindhoven', ST_GeomFromText('POINT(5.4697 51.4416)'), '2023-03-03');

INSERT INTO orders (customer_id, store_id, status, placed_at, notes)
SELECT 1 + (n * 7919) % 5000,
       IF(n % 4 = 0, NULL, 1 + n % 5),
       ELT(1 + (n * 31) % 6, 'pending','paid','shipped','delivered','refunded','cancelled'),
       TIMESTAMP('2024-01-01') + INTERVAL n * 13 MINUTE + INTERVAL (n % 1000) * 1000 MICROSECOND,
       IF(n % 9 = 0, 'Leave at the door', NULL)
FROM _seq WHERE n <= 60000;

INSERT INTO order_items (order_id, line_no, product_id, quantity, unit_price)
SELECT o.id, l.d + 1, 1 + (o.id * 17 + l.d * 29) % 400, 1 + (o.id + l.d) % 4, 0
FROM orders o JOIN _digits l ON l.d < 1 + o.id % 3;
UPDATE order_items i JOIN products p ON p.id = i.product_id SET i.unit_price = p.price;
UPDATE orders o JOIN (SELECT order_id, SUM(quantity * unit_price) s FROM order_items GROUP BY order_id) t ON t.order_id = o.id SET o.total = t.s;

INSERT INTO audit_events (happened_at, actor, action, payload)
SELECT TIMESTAMP('2025-01-01') + INTERVAL n HOUR, ELT(1 + n % 3, 'system','admin','api'), ELT(1 + n % 4, 'login','export','refund','update'),
       JSON_OBJECT('n', n) FROM _seq WHERE n <= 300;

DROP TABLE _seq;
DROP TABLE _digits;

CREATE VIEW customer_revenue AS
SELECT c.id, c.full_name, c.tier, COUNT(o.id) AS orders, COALESCE(SUM(o.total), 0) AS revenue
FROM customers c LEFT JOIN orders o ON o.customer_id = c.id AND o.status IN ('paid','shipped','delivered')
GROUP BY c.id, c.full_name, c.tier;

DELIMITER //
CREATE PROCEDURE restock(IN p_product INT, IN p_qty INT)
BEGIN
  IF p_qty <= 0 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'quantity must be positive';
  END IF;
  UPDATE products SET stock = stock + p_qty WHERE id = p_product;
  SELECT id, name, stock FROM products WHERE id = p_product;
END//
CREATE TRIGGER trg_orders_audit AFTER UPDATE ON orders FOR EACH ROW
BEGIN
  IF NEW.status <> OLD.status THEN
    INSERT INTO audit_events (happened_at, actor, action, payload)
    VALUES (NOW(), 'trigger', 'status', JSON_OBJECT('order', NEW.id, 'from', OLD.status, 'to', NEW.status));
  END IF;
END//
DELIMITER ;

CREATE DATABASE analytics;
CREATE TABLE analytics.daily_sales (day DATE PRIMARY KEY, orders INT NOT NULL, revenue DECIMAL(14,2) NOT NULL);
INSERT INTO analytics.daily_sales SELECT DATE(placed_at), COUNT(*), SUM(total) FROM shop.orders GROUP BY DATE(placed_at);
