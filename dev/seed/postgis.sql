-- Sample "city" database with PostGIS for Rowsmith development.
CREATE EXTENSION IF NOT EXISTS postgis;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE SCHEMA transit;

CREATE TYPE transit.mode AS ENUM ('bus', 'tram', 'metro', 'ferry');

CREATE TABLE districts (
  id serial PRIMARY KEY,
  name text NOT NULL UNIQUE,
  population integer NOT NULL CHECK (population >= 0),
  boundary geometry(Polygon, 4326) NOT NULL,
  founded date,
  tags text[] NOT NULL DEFAULT '{}'
);
COMMENT ON TABLE districts IS 'Administrative districts';
CREATE INDEX districts_boundary_gix ON districts USING gist (boundary);

CREATE TABLE transit.stations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  code varchar(8) NOT NULL UNIQUE,
  name text NOT NULL,
  mode transit.mode NOT NULL,
  district_id integer REFERENCES districts(id) ON DELETE SET NULL,
  location geometry(Point, 4326) NOT NULL,
  accessible boolean NOT NULL DEFAULT true,
  meta jsonb NOT NULL DEFAULT '{}'::jsonb,
  opened_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX stations_location_gix ON transit.stations USING gist (location);

CREATE TABLE transit.routes (
  id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name text NOT NULL,
  mode transit.mode NOT NULL,
  color char(7) NOT NULL DEFAULT '#3366ff',
  path geometry(LineString, 4326) NOT NULL,
  length_m double precision GENERATED ALWAYS AS (ST_Length(path::geography)) STORED
);

CREATE TABLE sensor_readings (
  sensor_id integer NOT NULL,
  observed_at timestamptz NOT NULL,
  pm25 numeric(6,2),
  temperature_c real,
  PRIMARY KEY (sensor_id, observed_at)
) PARTITION BY RANGE (observed_at);
CREATE TABLE sensor_readings_2025h1 PARTITION OF sensor_readings FOR VALUES FROM ('2025-01-01') TO ('2025-07-01');
CREATE TABLE sensor_readings_2025h2 PARTITION OF sensor_readings FOR VALUES FROM ('2025-07-01') TO ('2026-01-01');

CREATE TABLE incident_log (
  reported timestamptz NOT NULL,
  kind text NOT NULL,
  detail text,
  place geography(Point, 4326)
);
COMMENT ON TABLE incident_log IS 'No primary key: rows are edited by ctid';

-- A 6x5 grid of districts around a fictional city centre.
INSERT INTO districts (name, population, boundary, founded, tags)
SELECT 'District ' || chr(65 + x) || (y + 1),
       20000 + ((x * 7 + y * 13) % 17) * 3500,
       ST_MakeEnvelope(4.80 + x * 0.03, 52.30 + y * 0.02, 4.83 + x * 0.03, 52.32 + y * 0.02, 4326),
       date '1850-01-01' + ((x * 5 + y) * 1500),
       ARRAY['zone-' || (x % 3), CASE WHEN y % 2 = 0 THEN 'river' ELSE 'hill' END]
FROM generate_series(0, 5) x, generate_series(0, 4) y;

INSERT INTO transit.stations (code, name, mode, district_id, location, accessible, meta)
SELECT 'ST' || lpad(n::text, 4, '0'),
       (ARRAY['Central','Harbour','Museum','Park','Market','University','Stadium','Airport','Riverside','Old Mill'])[1 + n % 10] || ' ' || n,
       (ARRAY['bus','tram','metro','ferry']::transit.mode[])[1 + n % 4],
       1 + n % 30,
       ST_SetSRID(ST_MakePoint(4.80 + random() * 0.18, 52.30 + random() * 0.10), 4326),
       n % 7 <> 0,
       jsonb_build_object('platforms', 1 + n % 4, 'shelter', n % 2 = 0)
FROM generate_series(1, 400) n;

INSERT INTO transit.routes (name, mode, color, path)
SELECT 'Line ' || r, (ARRAY['bus','tram','metro','ferry']::transit.mode[])[1 + r % 4],
       (ARRAY['#e4572e','#29335c','#f3a712','#669bbc','#a8c686','#8e5572'])[1 + r % 6],
       ST_MakeLine(ARRAY(SELECT ST_SetSRID(ST_MakePoint(4.80 + (i * 0.02) , 52.30 + r * 0.008 + sin(i + r) * 0.01), 4326)
                         FROM generate_series(0, 9) i))
FROM generate_series(1, 12) r;

INSERT INTO sensor_readings
SELECT s, ts, round((5 + random() * 40)::numeric, 2), (random() * 25)::real
FROM generate_series(1, 20) s, generate_series('2025-01-01'::timestamptz, '2025-12-31', interval '6 hours') ts;

INSERT INTO incident_log
SELECT now() - (n || ' hours')::interval, (ARRAY['delay','closure','accident','maintenance'])[1 + n % 4],
       'Reported by patrol ' || n, ST_SetSRID(ST_MakePoint(4.8 + random() * 0.18, 52.3 + random() * 0.1), 4326)::geography
FROM generate_series(1, 120) n;

CREATE MATERIALIZED VIEW transit.station_density AS
SELECT d.id, d.name, count(s.id) AS stations, round(count(s.id)::numeric / NULLIF(d.population, 0) * 10000, 2) AS per_10k
FROM districts d LEFT JOIN transit.stations s ON s.district_id = d.id GROUP BY d.id, d.name, d.population;

CREATE VIEW transit.accessible_stations AS SELECT id, code, name, mode, location FROM transit.stations WHERE accessible;

CREATE FUNCTION transit.nearest_stations(lon double precision, lat double precision, k integer DEFAULT 5)
RETURNS TABLE (code varchar, name text, meters double precision) LANGUAGE sql STABLE AS $$
  SELECT s.code, s.name, ST_Distance(s.location::geography, ST_SetSRID(ST_MakePoint(lon, lat), 4326)::geography)
  FROM transit.stations s ORDER BY s.location <-> ST_SetSRID(ST_MakePoint(lon, lat), 4326) LIMIT k
$$;

CREATE FUNCTION touch_founded() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.founded IS NULL THEN NEW.founded := current_date; END IF;
  RAISE NOTICE 'district % saved', NEW.name;
  RETURN NEW;
END $$;
CREATE TRIGGER districts_touch BEFORE INSERT OR UPDATE ON districts FOR EACH ROW EXECUTE FUNCTION touch_founded();
ANALYZE;
