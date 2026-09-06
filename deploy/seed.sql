-- Target databases for a local demo.
--
-- shop_staging and shop_prod are deliberately *not* identical: staging has a
-- column and an index production lacks. Pairing them for comparison surfaces
-- that immediately, which is the whole point of peer drift detection — it works
-- with two connection strings and no repository at all.

CREATE DATABASE shop_prod;
CREATE DATABASE shop_staging;
CREATE DATABASE analytics;

\connect shop_prod

CREATE TYPE order_status AS ENUM ('pending', 'paid', 'shipped');

CREATE TABLE customers (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id bigint NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    status      order_status NOT NULL DEFAULT 'pending',
    total       numeric(12,2) NOT NULL DEFAULT 0 CHECK (total >= 0),
    placed_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX orders_customer_idx ON orders (customer_id, placed_at DESC);
COMMENT ON TABLE orders IS 'Customer orders';

\connect shop_staging

CREATE TYPE order_status AS ENUM ('pending', 'paid', 'shipped');

CREATE TABLE customers (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id bigint NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    status      order_status NOT NULL DEFAULT 'pending',
    total       numeric(12,2) NOT NULL DEFAULT 0 CHECK (total >= 0),
    placed_at   timestamptz NOT NULL DEFAULT now(),
    -- Present in staging, absent in production: this is the drift.
    channel     text
);

CREATE INDEX orders_customer_idx ON orders (customer_id, placed_at DESC);
CREATE INDEX orders_channel_idx  ON orders (channel) WHERE channel IS NOT NULL;
COMMENT ON TABLE orders IS 'Customer orders';

\connect analytics

CREATE TABLE daily_totals (
    day       date PRIMARY KEY,
    orders    bigint NOT NULL DEFAULT 0,
    revenue   numeric(14,2) NOT NULL DEFAULT 0
);
