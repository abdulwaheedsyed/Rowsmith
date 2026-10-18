-- Sample "analytics" database for ClickHouse: the shapes a ClickHouse
-- database usually has (wide event tables, partitions, a materialized view
-- feeding a summary table, a dictionary) and the types worth testing.
CREATE DATABASE IF NOT EXISTS analytics COMMENT 'Product analytics';

CREATE TABLE analytics.users
(
    id UInt64,
    email String,
    full_name String,
    plan Enum8('free' = 1, 'pro' = 2, 'enterprise' = 3) DEFAULT 'free',
    country FixedString(2),
    referrer Nullable(String),
    signup_date Date,
    updated_at DateTime DEFAULT now() COMMENT 'Newest version wins on merge'
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY id
COMMENT 'One row per user; ReplacingMergeTree keeps the newest version';

INSERT INTO analytics.users
SELECT
    number + 1,
    concat('user', toString(number + 1), '@example.com'),
    concat(['Ada', 'Grace', 'Alan', 'Edsger', 'Barbara', 'Donald'][number % 6 + 1], ' ', ['Lovelace', 'Hopper', 'Turing', 'Dijkstra', 'Liskov', 'Knuth'][intDiv(number, 6) % 6 + 1]),
    ['free', 'free', 'pro', 'enterprise'][number % 4 + 1],
    ['US', 'GB', 'DE', 'SA', 'IN', 'JP'][number % 6 + 1],
    if(number % 3 = 0, NULL, ['search', 'newsletter', 'friend'][number % 3 + 1]),
    toDate('2024-01-01') + (number % 600),
    toDateTime('2025-06-01 00:00:00') + number
FROM numbers(2000);

CREATE TABLE analytics.events
(
    event_time DateTime64(3, 'UTC'),
    event_date Date MATERIALIZED toDate(event_time),
    user_id UInt64,
    session_id UUID,
    event_type LowCardinality(String),
    page String,
    device Enum8('desktop' = 1, 'mobile' = 2, 'tablet' = 3),
    country LowCardinality(String),
    revenue Decimal(12, 2) DEFAULT 0,
    duration_ms UInt32 CODEC(T64, ZSTD(1)),
    is_bot Bool DEFAULT false,
    ip IPv4,
    tags Array(String),
    properties Map(String, String),
    INDEX idx_page page TYPE bloom_filter GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(event_time)
ORDER BY (event_type, event_time)
TTL toDateTime(event_time) + INTERVAL 5 YEAR
COMMENT 'Clickstream events, partitioned by month';

INSERT INTO analytics.events (event_time, user_id, session_id, event_type, page, device, country, revenue, duration_ms, is_bot, ip, tags, properties)
SELECT
    toDateTime64('2025-01-01 00:00:00', 3, 'UTC') + toIntervalSecond(number * 97),
    number % 2000 + 1,
    generateUUIDv4(number),
    ['page_view', 'page_view', 'page_view', 'click', 'signup', 'purchase'][number % 6 + 1],
    ['/', '/pricing', '/docs', '/blog', '/checkout'][number % 5 + 1],
    ['desktop', 'mobile', 'tablet'][number % 3 + 1],
    ['US', 'GB', 'DE', 'SA', 'IN', 'JP'][number % 6 + 1],
    if(number % 6 = 5, toDecimal64((number % 400) + 0.99, 2), 0),
    (number * 37) % 30000,
    number % 50 = 0,
    toIPv4(concat('10.', toString(number % 256), '.', toString(intDiv(number, 256) % 256), '.', toString(number % 199 + 1))),
    arraySlice(['new', 'returning', 'campaign', 'mobile-app'], 1, number % 4),
    map('utm_source', ['google', 'twitter', 'direct'][number % 3 + 1], 'ab', toString(number % 2))
FROM numbers(200000);

-- Revenue per day and country, kept up to date by a materialized view.
CREATE TABLE analytics.daily_revenue
(
    day Date,
    country LowCardinality(String),
    purchases UInt64,
    revenue Decimal(18, 2)
)
ENGINE = SummingMergeTree
ORDER BY (day, country);

CREATE MATERIALIZED VIEW analytics.daily_revenue_mv TO analytics.daily_revenue AS
SELECT toDate(event_time) AS day, country, count() AS purchases, sum(revenue) AS revenue
FROM analytics.events
WHERE event_type = 'purchase'
GROUP BY day, country;

INSERT INTO analytics.daily_revenue
SELECT toDate(event_time), country, count(), sum(revenue) FROM analytics.events WHERE event_type = 'purchase' GROUP BY 1, 2;

CREATE VIEW analytics.top_pages AS
SELECT page, count() AS views, uniqExact(user_id) AS visitors
FROM analytics.events
WHERE event_type = 'page_view'
GROUP BY page
ORDER BY views DESC;

CREATE TABLE analytics.countries
(
    code String,
    name String
)
ENGINE = MergeTree
ORDER BY code;

INSERT INTO analytics.countries VALUES ('US', 'United States'), ('GB', 'United Kingdom'), ('DE', 'Germany'), ('SA', 'Saudi Arabia'), ('IN', 'India'), ('JP', 'Japan');

CREATE DICTIONARY analytics.country_names
(
    code String,
    name String
)
PRIMARY KEY code
SOURCE(CLICKHOUSE(TABLE 'countries' DB 'analytics'))
LIFETIME(MIN 300 MAX 600)
LAYOUT(COMPLEX_KEY_HASHED());

-- Every type family the grid has to render and edit.
CREATE TABLE analytics.zoo
(
    id UInt32,
    i8 Int8,
    u64 UInt64,
    i128 Int128,
    u256 UInt256,
    f32 Float32,
    f64 Float64,
    dec Decimal(38, 10),
    s String,
    fs FixedString(4),
    lc LowCardinality(Nullable(String)),
    n Nullable(Int32),
    b Bool,
    d Date,
    d32 Date32,
    dt DateTime('Asia/Riyadh'),
    dt64 DateTime64(6),
    t Time,
    u UUID,
    ip4 IPv4,
    ip6 IPv6,
    e Enum16('a' = 1, 'b' = 1000),
    arr Array(Nullable(Int32)),
    tup Tuple(name String, n UInt8),
    m Map(String, UInt64),
    nested Nested(k String, v Float64),
    j JSON,
    pt Point,
    poly Polygon
)
ENGINE = MergeTree
ORDER BY id;

INSERT INTO analytics.zoo VALUES
    (1, -8, 18446744073709551615, -170141183460469231731687303715884105728, 115792089237316195423570985008687907853269984665640564039457584007913129639935,
     0.1, 3.141592653589793, 1234567890.0123456789, 'hello\nworld', 'abcd', 'low', NULL, true, '2025-02-28', '1925-01-01',
     '2025-06-01 12:00:00', '2025-06-01 12:00:00.123456', '12:34:56', '7c9e6679-7425-40de-944b-e07fc1f90ae7', '192.168.1.10', '2001:db8::1',
     'b', [1, NULL, 3], ('ada', 36), map('a', 1, 'b', 2), ['x', 'y'], [1.5, 2.5], '{"plan": "pro", "seats": 5, "tags": ["a", "b"]}',
     (46.6753, 24.7136), [[(0, 0), (10, 0), (10, 10), (0, 10), (0, 0)]]),
    (2, 0, 0, 0, 0, 0, 0, 0, '', '\0\0\0\0', NULL, 42, false, '1970-01-01', '1970-01-01', '1970-01-01 03:00:00', '1970-01-01 00:00:00', '00:00:00',
     '00000000-0000-0000-0000-000000000000', '0.0.0.0', '::', 'a', [], ('', 0), map(), [], [], '{}', (0, 0), []);

-- A small table for row edits.
CREATE TABLE analytics.notes
(
    id UInt32,
    body String,
    created DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY id;

INSERT INTO analytics.notes (id, body) VALUES (1, 'first'), (2, 'second'), (3, 'third');
