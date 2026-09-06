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
schemaver admin <metadata-url> you@example.com 'a-long-enough-password' 'Your Name'
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

## Demo mode

`SCHEMAVER_DEMO_MODE=1` seeds a read-only account and advertises its credentials
on the sign-in page, so a public demo needs no log access to get in. The account
is a **viewer**: it can read every schema and divergence but cannot register a
database or store a credential, so an exposed demo cannot be turned into a
credential-harvesting page. Every page carries a banner while it is on.

Off by default, and it publishes a password on purpose — never enable it on a
deployment connected to anything real.

## Commands

```
schemaver introspect  <url>    Canonical schema as JSON
schemaver fingerprint <url>    Version digest
schemaver ddl         <url>    Schema as executable DDL
schemaver databases   <url>    Databases on a server
schemaver scan        <url>    Read every database on a server

schemaver run      <metadata-url>    Observation loop and interface
schemaver register <metadata-url> <target-url> [name]
schemaver pair     <metadata-url> <instance-id> <database> <peer>
schemaver migrate  <metadata-url>    Apply schemaver's own schema
schemaver admin    <metadata-url> <email> <password> [name]
```

## Design

`decisions.md` records every decision that constrains the build, with the
alternatives and what each one takes off the table. `product.md` is the
requirement set, split by whether a capability writes to a managed database.
