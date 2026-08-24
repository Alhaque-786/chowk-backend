-- 0016_lock_out_supabase_data_api
--
-- Why this change:
--
-- Moving Postgres to Supabase attaches something this schema was never designed
-- for: a public HTTP API in front of it. Supabase runs PostgREST at
-- https://<project-ref>.supabase.co/rest/v1/<table> over the `public` schema, and
-- it is reachable by anyone holding the project's PUBLISHABLE key -- a key that is
-- meant to be public, shipped in browser bundles and mobile apps by design.
--
-- What normally stops that from being a data breach is Row Level Security. This
-- schema has none. All 37 tables are created by these migrations in `public` with
-- no RLS and no policies, because they were written for a database that only ever
-- had one client: this Go service, over a private connection.
--
-- So the combination is the problem, not either half. On a self-hosted Postgres,
-- "no RLS" is correct and unremarkable -- authorisation lives in the application,
-- which is where this codebase puts it. Bolt a public REST API onto the same
-- database and the same schema means `users`, `addresses`, `orders`,
-- `ledger_entries` and `payments` may be readable with a key that is not a secret.
--
-- THE FIX IS TO REMOVE THE GRANT, not to add RLS to 37 tables.
--
-- RLS would be the answer if we wanted PostgREST to work with restrictions. We do
-- not want PostgREST at all: every read and write in this system goes through the
-- Go API, which owns authentication, tenant scoping and the audit log. Writing
-- policies for an interface nobody uses would be a large amount of security-
-- critical SQL maintained forever to protect a door we would rather brick up.
--
-- Revoking is also the stronger guarantee. A missing policy fails OPEN on a table
-- with RLS switched off; a missing GRANT fails CLOSED. If somebody adds a table in
-- a later migration and forgets to think about this, the default privileges below
-- cover it.
--
-- WHY A MIGRATION AND NOT THE DASHBOARD. Supabase has a setting for this --
-- Settings > API > Exposed schemas, remove `public` -- and it is worth setting as
-- well. But it is a checkbox in a web UI with no review, no history, and nothing
-- stopping someone re-adding `public` while debugging. This file is in git, runs on
-- every environment, and states its reasoning.
--
-- Local development is unaffected: the roles below only exist on Supabase, so
-- every statement is guarded and this migration is a no-op against a plain
-- Postgres container.

DO $$
DECLARE
  -- The two roles PostgREST authenticates as. `anon` is the publishable key,
  -- `authenticated` is a Supabase Auth session -- which this system does not use,
  -- since it issues its own JWTs.
  api_role TEXT;
BEGIN
  FOREACH api_role IN ARRAY ARRAY['anon', 'authenticated'] LOOP
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = api_role) THEN
      RAISE NOTICE 'role % absent (not a Supabase database); nothing to revoke', api_role;
      CONTINUE;
    END IF;

    -- Existing objects. TABLES covers views too, which matters: a view over a
    -- protected table is otherwise a way around the revoke.
    EXECUTE format('REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %I', api_role);
    EXECUTE format('REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM %I', api_role);
    EXECUTE format('REVOKE ALL ON ALL FUNCTIONS IN SCHEMA public FROM %I', api_role);
    -- Without this the role can still enumerate and reach objects it was granted
    -- individually, and USAGE is what PostgREST needs to introspect the schema at
    -- all.
    EXECUTE format('REVOKE USAGE ON SCHEMA public FROM %I', api_role);

    -- Future objects. This is the half that protects the NEXT migration, written
    -- by someone who has never read this file. Applied for both the role running
    -- migrations and postgres, because default privileges attach to the granting
    -- role, not the schema.
    EXECUTE format(
      'ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM %I', api_role);
    EXECUTE format(
      'ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON SEQUENCES FROM %I', api_role);
    EXECUTE format(
      'ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON FUNCTIONS FROM %I', api_role);

    RAISE NOTICE 'revoked public-schema access from role %', api_role;
  END LOOP;
END
$$;
