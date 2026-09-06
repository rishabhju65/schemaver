# schemaver

Version control for database schemas — track, diff, review and roll out schema
changes like code.

PostgreSQL only, deliberately (see `decisions.md`, D-003).

## Run it locally

```sh
docker compose up --build
```

That starts one Postgres serving three roles — schemaver's metadata, the shadow
databases it needs to prove migrations, and three seeded demo databases to
watch — plus schemaver itself on http://localhost:8080.

Register the demo server with itself and pair two databases so drift shows up:

```sh
export M='postgres://schemaver:schemaver@localhost:5432/schemaver?sslmode=disable'

docker compose exec schemaver /schemaver register "$M" \
  'postgres://schemaver:schemaver@postgres:5432/postgres?sslmode=disable' demo

docker compose exec schemaver /schemaver pair "$M" 1 shop_staging shop_prod
```

`shop_staging` has a column and an index that `shop_prod` lacks, so the drift
page has something to show within one observation cycle.

## What it does today

Reads a Postgres server, records every schema it finds, and reports when one
diverges from what it should be.

- **Introspection** into a canonical, engine-neutral model, with a
  content-addressed version — identical schemas always produce the same digest,
  however they were written.
- **Observation loop** across many instances at once, capped per instance so a
  server hosting forty databases never receives forty connections. A cheap probe
  avoids a full read when nothing changed; a periodic full read covers the
  probe's blind spots.
- **Drift detection** against a declared schema, or against another live
  database — which works with two connection strings and no repository at all.
- **History**: every observed change, with an object-level delta showing which
  tables, enums and sequences differ.
- **Shadow databases** for reading declared schemas and proving a migration
  produces the schema it claims.

Applying migrations is not built. The semantic diff engine it depends on does not
exist yet, so every change schemaver currently sees arrived from outside it.

## Reading a database needs almost nothing

Introspection reads `pg_catalog`, which Postgres exposes to every role. The
account schemaver needs cannot read a single row of your data:

```sql
CREATE ROLE schemaver LOGIN PASSWORD '...';
GRANT CONNECT ON DATABASE app TO schemaver;
```

## Before deploying this anywhere public

**The first account is created with a one-time token.** On a fresh deployment
with no administrator, schemaver prints a setup token to its log and every page
redirects to `/setup` until it is used. The token lives only in memory, so
restarting issues a new one and there is nothing on disk to leak.

```sh
docker compose logs schemaver | grep token=
```

Prefer to skip the web flow entirely:

```sh
schemaver account <metadata-url> 'Your Team' you@example.com 'a-long-enough-password'
```

Every page carrying schema information requires an account. Three roles: `admin`
manages accounts and instances, `operator` registers instances, `viewer` reads
only.

**Back up the encryption key.** `SCHEMAVER_ENCRYPTION_KEY` encrypts stored
credentials and is never written to the database. Lose it and every stored
credential is unrecoverable.

```sh
openssl rand -base64 32
```

**schemaver needs CREATEDB on its own Postgres.** Shadow databases are created
and dropped there. Managed Postgres offerings that hand you a single database
cannot host schemaver's metadata — though they are perfectly fine as *targets*
to observe.

## Accounts

An **account** owns database servers, credentials and environments. Users belong
to an account, and nothing one account owns is reachable from another — enforced
by a store that cannot express an unscoped query, so a missing filter is not a
mistake that can be made quietly.

`SCHEMAVER_OPEN_SIGNUP=1` lets anyone create an account. Because accounts are
isolated, that grants access to nothing already registered.

It does mean strangers can ask this server to open connections, so with open
sign-up on, **private and link-local addresses are refused** — including cloud
instance metadata at `169.254.169.254`, which on some providers hands out the
host's own credentials. Addresses are judged after resolution, never by
hostname, since pointing a public name at an internal address is the standard
way that check is defeated.

Set `SCHEMAVER_ALLOW_PRIVATE_TARGETS=1` to override, which you will need if you
run open sign-up and legitimately manage private databases. Doing both at once
is logged as a warning, because it makes this server a probe of its own network.

With open sign-up off — the default — reaching private addresses is permitted,
since that is the entire point of a self-hosted deployment, and the first account
is created with a one-time token printed to the log.

## Commands

```
schemaver introspect  <url>    Canonical schema as JSON
schemaver fingerprint <url>    Version digest
schemaver ddl         <url>    Schema as executable DDL
schemaver databases   <url>    Databases on a server
schemaver scan        <url>    Read every database on a server

schemaver run      <metadata-url>    Observation loop and interface
schemaver migrate  <metadata-url>    Apply schemaver's own schema
schemaver account  <metadata-url> <name> <email> <password>
```

## Design

`decisions.md` records every decision that constrains the build, with the
alternatives and what each one takes off the table. `product.md` is the
requirement set, split by whether a capability writes to a managed database.
